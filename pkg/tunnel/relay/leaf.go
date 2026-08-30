// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package relay 提供中继叶子侧的流接收与分发逻辑。
//
// 叶子节点（如 sclient relay start / portal）连接 hub 后，通过一条 mux
// 接收到达的流。每条流的首帧是 [4B big-endian length][json]：
//
//   - {"dial":"addr"} → 任意 TCP 流中继（出口模式，--dial-allow）
//   - 否则 → 隧道 HTTP 元数据（tunnel.Request），转发到本地 HTTP 服务
//
// 该包同时被 cmd/sclient 与 pkg/server 测试复用，避免逻辑重复。
package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// pumpGracePeriod 是 pump 第二方向完成收尾的宽限期：第一方向完成（已传播
// 半关闭）后，第二方向需在此时间内完成；超时视为对端非合作，强制关闭两端
// 防 goroutine / FD 泄漏。长连接（双向持续活跃）不触发宽限期——计时器只在
// 某方向完成且另一方向仍空闲时启动。
const pumpGracePeriod = 60 * time.Second

// ServeOptions 配置 Serve 的拨号策略。
type ServeOptions struct {
	// DialPolicy 出口模式下的目标地址校验 + 解析策略。
	// 返回 (resolvedAddr, ok)：ok 表示放行，resolvedAddr 是实际应拨的地址
	// （主机名会解析为具体 IP，防 DNS rebinding TOCTOU）。
	// nil 时使用 DialAllowed（严格：仅允许公网目标）。
	DialPolicy func(addr string) (resolvedAddr string, ok bool)

	// DialResultFrames 控制叶子在出口拨号后是否向对端回写拨号结果帧
	// [4B len][{"dial_result":"ok"}] / [{"dial_result":"error","message":...}]（I27）。
	// hub 的 /api/relay/stream 在写 200 前读取该帧，据此返回 200/502/504，
	// 使 200 语义变为「数据面就绪」而非「已受理」。
	// 仅经 hub 中继的 relay start 需开启；webrtc 直连（p2p listen）必须保持
	// false——否则结果帧会被对端当远程数据透传，污染数据流。
	DialResultFrames bool
}

// Serve 是叶子侧的流接收循环。
// localAddr 是本地 HTTP 服务地址（HTTP 中继转发目标）；
// dialAllow 为 true 时启用出口模式（收到 dial 帧可出站连接）。
func Serve(ctx context.Context, m *mux.Mux, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger, opts ...ServeOptions) error {
	if logger == nil {
		logger = slog.Default()
	}
	sOpts := ServeOptions{}
	for _, o := range opts {
		if o.DialPolicy != nil {
			sOpts.DialPolicy = o.DialPolicy
		}
		if o.DialResultFrames {
			sOpts.DialResultFrames = true
		}
	}
	dialPolicy := DialAllowed
	if sOpts.DialPolicy != nil {
		dialPolicy = sOpts.DialPolicy
	}
	for {
		stream, err := m.Accept(ctx)
		if err != nil {
			return err
		}
		go func(s mux.Stream, m *mux.Mux) {
			// 每流 goroutine 处理不可信输入，panic 会击穿到整个进程（叶子被
			// 恶意对端 DoS 的路径）——兜底 recover 防止进程崩溃。
			defer func() {
				if r := recover(); r != nil {
					logger.Error("中继流处理 panic", "panic", r)
				}
			}()
			defer s.Close()

			lenBuf := make([]byte, 4)
			if _, rerr := io.ReadFull(s, lenBuf); rerr != nil {
				logger.Warn("中继流: 读首帧长度失败", "error", rerr)
				return
			}
			metaLen := binary.BigEndian.Uint32(lenBuf)
			if metaLen == 0 || metaLen > tunnel.MaxMetadataBytes {
				logger.Warn("中继流: 非法帧长度", "len", metaLen)
				return
			}
			meta := make([]byte, metaLen)
			if _, rerr := io.ReadFull(s, meta); rerr != nil {
				logger.Warn("中继流: 读首帧内容失败", "error", rerr)
				return
			}

			// 先按 UDP 映射帧解析（首帧 {"udp": addr} → 该 mux 作为 UDP 数据报通道）。
			var ur hub.UDPRequest
			if err := json.Unmarshal(meta, &ur); err == nil && ur.UDP != "" {
				if !dialAllow {
					logger.Warn("收到 UDP 映射帧但未开启 --dial-allow", "addr", ur.UDP)
					return
				}
				handleUDPMap(ctx, m, s, ur.UDP, dialPolicy, logger)
				return
			}
			// 再按 dial 帧解析
			var d hub.DialRequest
			if err := json.Unmarshal(meta, &d); err == nil && d.Dial != "" {
				if !dialAllow {
					logger.Warn("收到 dial 帧但未开启 --dial-allow", "addr", d.Dial)
					if sOpts.DialResultFrames {
						_ = writeDialResultFrame(s, &hub.DialResultFrame{DialResult: hub.DialResultError, Message: "未开启 --dial-allow"})
					}
					return
				}
				// 策略返回实际应拨的地址（已解析 IP，防 DNS rebinding TOCTOU）
				resolved, ok := dialPolicy(d.Dial)
				if !ok {
					logger.Warn("出口模式收到非法 dial 地址", "addr", d.Dial)
					if sOpts.DialResultFrames {
						_ = writeDialResultFrame(s, &hub.DialResultFrame{DialResult: hub.DialResultError, Message: "地址未通过拨号策略"})
					}
					return
				}
				dialAddr := resolved
				if dialAddr == "" {
					dialAddr = d.Dial
				}
				logger.Info("出口拨号", "addr", d.Dial, "dial", dialAddr)
				remote, derr := net.DialTimeout("tcp", dialAddr, 10*time.Second)
				if derr != nil {
					logger.Warn("出口拨号失败", "addr", d.Dial, "error", derr)
					if sOpts.DialResultFrames {
						_ = writeDialResultFrame(s, &hub.DialResultFrame{DialResult: hub.DialResultError, Message: derr.Error()})
					}
					return
				}
				defer remote.Close()
				// 记录拨号成功：让对端（mesh connect）与运维可确认出口数据通路就绪。
				if sOpts.DialResultFrames {
					// 先回写 ok 结果帧，hub 读到后才返回 200；随后进入 pump，数据面就绪。
					if werr := writeDialResultFrame(s, &hub.DialResultFrame{DialResult: hub.DialResultOK}); werr != nil {
						logger.Warn("写拨号结果帧失败", "addr", d.Dial, "error", werr)
					}
				}
				logger.Info("出口拨号成功，开始泵送", "addr", d.Dial, "remote", remote.RemoteAddr().String())
				pump(s, remote, pumpGracePeriod)
				logger.Info("出口泵送结束", "addr", d.Dial)
				return
			}

			// 否则按隧道 HTTP 中继处理
			var req tunnel.Request
			if err := json.Unmarshal(meta, &req); err != nil || req.Method == "" {
				logger.Warn("无法解析的中继帧", "meta", string(meta))
				return
			}
			serveHTTP(ctx, s, localAddr, req, httpClient, logger)
		}(stream, m)
	}
}

// handleUDPMap 处理 UDP 端口映射流（首帧 {"udp": addr}）：把该 mux 作为 UDP 数据报
// 通道（FrameDatagram）。收到数据报 → 转发到目标 UDP 地址（net.DialUDP 连接 socket）；
// 目标 UDP 响应 → SendDatagram 回传（flowID 0，单端口映射）。控制流关闭/mux 关闭时
// 停止（对端 sclient udp map 退出）。
//
// 安全边界：目标地址由对端指定（sclient udp map --remote），叶子作为 UDP 出口——
// 与 TCP dial 帧同属"出口模式"，由 mesh node 的 --dial-allow 语义约束；udp map 目标
// 地址应经调用方校验（sclient udp map 本地侧默认仅允许 --remote 指定地址）。
func handleUDPMap(ctx context.Context, m *mux.Mux, control mux.Stream, udpAddr string, dialPolicy func(string) (string, bool), logger *slog.Logger) {
	// 与 TCP dial 帧一致：目标须通过拨号策略（DialAllowed/NewServiceDialPolicy），
	// 防 --dial-allow 节点被任意 mesh 对端当任意内网 UDP 转发代理（SSRF）。
	// 策略返回实际应拨地址（主机名解析为 IP，防 DNS rebinding TOCTOU）。
	resolved, ok := dialPolicy(udpAddr)
	if !ok {
		logger.Warn("UDP 映射地址未通过拨号策略", "addr", udpAddr)
		return
	}
	dialAddr := resolved
	if dialAddr == "" {
		dialAddr = udpAddr
	}
	raddr, err := net.ResolveUDPAddr("udp", dialAddr)
	if err != nil {
		logger.Warn("UDP 映射目标地址非法", "addr", udpAddr, "error", err)
		return
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		logger.Warn("UDP 映射连接失败", "addr", udpAddr, "error", err)
		return
	}
	logger.Info("UDP 端口映射就绪", "target", udpAddr, "local", conn.LocalAddr().String())

	// 单 UDP 协程串行处理转发与响应（避免 handler/读/关闭并发访问 conn 的竞态）：
	// 读用短 deadline 周期性让出给待发数据（否则阻塞在 conn.Read 无法消费 sendCh）。
	// 数据报经非阻塞通道投递，通道满则丢弃（UDP 语义，背压自然丢包）。
	// stop 关闭（控制流 EOF/对端退出）→ 协程退出，防优雅关闭后 goroutine/FD 泄漏。
	sendCh := make(chan []byte, 64)
	stop := make(chan struct{})
	udpDone := make(chan struct{})
	go func() {
		defer close(udpDone)
		defer func() { _ = conn.Close() }()
		buf := make([]byte, mux.MaxDatagramPayload)
		for {
			select {
			case <-stop:
				return
			case data := <-sendCh:
				if _, werr := conn.Write(data); werr != nil {
					// 超限/瞬时写失败：丢弃该数据报并继续（防恶意超长数据报终止映射；
					// conn 真正关闭时读路径也会失败退出）。
					logger.Debug("UDP 转发失败（丢弃该数据报）", "error", werr)
				}
				continue // 优先排空待发数据
			default:
			}
			// 非阻塞读响应：50ms deadline 让出给 sendCh/stop（本地 UDP 转发延迟可忽略）。
			_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, rerr := conn.Read(buf)
			if rerr != nil {
				var ne *net.OpError
				if errors.As(rerr, &ne) && ne.Timeout() {
					continue // 无响应，回 select 检查待发/stop
				}
				return
			}
			if serr := m.SendDatagram(0, buf[:n]); serr != nil {
				return
			}
		}
	}()

	m.SetDatagramHandler(func(flowID uint32, data []byte) {
		select {
		case sendCh <- data:
		default:
		}
	})
	defer func() { m.SetDatagramHandler(nil) }()

	// 控制流读到 EOF（对端 sclient udp map 退出）→ 停止转发。
	var one [1]byte
	_, _ = control.Read(one[:])
	_ = ctx
	// 先清 handler（不再投递 sendCh）→ 通知 UDP 协程停止 → 有界回收（≤50ms read deadline）。
	m.SetDatagramHandler(nil)
	close(stop)
	<-udpDone
	_ = control.Close()
}

// serveHTTP 处理隧道 HTTP 中继流（metadata 已解析）。
//
// 安全边界（I20）：req.URL 是不可信输入，绝不能与 localAddr 直接拼接——否则
// 攻击者可注入 `@evil.com:443/x` 劫持 URL 的 Host/Userinfo，把叶子变成不受限
// HTTP 代理（SSRF）。这里固定用 url.Parse(localAddr) 得到 base，只把请求的相对
// 路径 / query 覆盖到 base 的字段上，从不拼接原始字符串；绝对 URL、带 host、
// 带 userinfo、带 opaque 的请求一律拒绝（返回 400）。
func serveHTTP(ctx context.Context, s mux.Stream, localAddr string, req tunnel.Request, httpClient *http.Client, logger *slog.Logger) {
	if httpClient == nil {
		httpClient = http.DefaultClient // S29：防御性兜底
	}
	base, err := url.Parse(localAddr)
	if err != nil || base.Scheme == "" || base.Host == "" {
		logger.Warn("本地服务地址无效", "localAddr", localAddr)
		writeErrorResponse(s, http.StatusInternalServerError)
		return
	}
	rel, err := url.Parse(req.URL)
	if err != nil || rel.IsAbs() || rel.Host != "" || rel.User != nil || rel.Opaque != "" {
		// 仅允许相对路径；绝对 URL / host / userinfo / opaque 一律拒绝（防 SSRF host 劫持）。
		logger.Warn("拒绝非法中继 URL（仅允许相对路径）", "url", req.URL)
		writeErrorResponse(s, http.StatusBadRequest)
		return
	}
	base.Path = rel.Path
	base.RawPath = rel.RawPath // 保留原始转义（如 %2F 与 Path 语义一致）
	base.RawQuery = rel.RawQuery
	base.Fragment = "" // Fragment 不随请求发送
	forwardURL := base.String()

	// body 处理：仅对允许带 body 的方法把流 s 作为请求体（NopCloser 避免
	// http.Client 读完 body 后 Close(s) 关掉整条 mux 流）。GET/HEAD 等无 body
	// 方法不设 body，否则 http.Client 会尝试读 s 到 EOF 干扰协议。
	var bodyReader io.Reader
	if methodAllowsBody(req.Method) {
		bodyReader = io.NopCloser(s)
	}
	localReq, err := http.NewRequestWithContext(ctx, req.Method, forwardURL, bodyReader) //nolint:gosec // G704: SSRF is intentional (relay proxy)
	if err != nil {
		return
	}
	for k, v := range req.Headers {
		localReq.Header.Set(k, v)
	}

	resp, err := httpClient.Do(localReq)
	if err != nil {
		logger.Warn("转发到本地失败", "path", req.URL, "error", err)
		return
	}
	defer resp.Body.Close()

	// 回写响应 metadata + body（stream 直达，不缓冲）
	respMeta := tunnel.Response{
		Proto:         "HTTP/1.1",
		Status:        resp.StatusCode,
		Headers:       resp.Header,
		ContentLength: resp.ContentLength,
	}
	respMetaJSON, _ := json.Marshal(respMeta)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(respMetaJSON)))
	_, _ = s.Write(lenBuf)
	_, _ = s.Write(respMetaJSON)
	_, _ = io.Copy(s, resp.Body)
}

// writeErrorResponse 向流回写一个错误 HTTP 响应 metadata（无 body，ContentLength=0）。
// 协议与 serveHTTP 成功路径一致：[4B BE len][tunnel.Response JSON]。
func writeErrorResponse(s mux.Stream, status int) {
	respMeta := tunnel.Response{
		Proto:   "HTTP/1.1",
		Status:  status,
		Headers: http.Header{},
	}
	respMetaJSON, _ := json.Marshal(respMeta)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(respMetaJSON)))
	_, _ = s.Write(lenBuf)
	_, _ = s.Write(respMetaJSON)
}

// writeDialResultFrame 向流回写拨号结果帧 [4B len][{"dial_result":...}]（I27）。
// 协议与 dial 帧一致：[4B big-endian length][JSON]。仅当 ServeOptions.DialResultFrames
// 为 true 时调用；webrtc 直连（p2p listen）不写结果帧，避免污染数据流。
func writeDialResultFrame(s mux.Stream, result *hub.DialResultFrame) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(b)))
	if err := writeFull(s, lenBuf); err != nil {
		return err
	}
	return writeFull(s, b)
}

// writeFull 循环写满整个 buf，处理流的部分写（mux 流在发送窗口小于 buf 长度时
// 返回 n<len 的短写）。仅用于小帧（长度前缀 + 元数据）；数据面泵送用 io.Copy。
func writeFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

// methodAllowsBody 报告 HTTP 方法是否允许携带请求体。
// 仅对允许 body 的方法把隧道流作为请求体；GET/HEAD 等无 body 方法不设 body，
// 否则 http.Client 会尝试读流到 EOF，干扰后续响应写入。
// 注意：DELETE/OPTIONS 语义上可携带 body（RFC 7231），一并纳入，否则 body 字节
// 会残留在流上被静默丢弃；不加入 GET/HEAD 以防御"客户端不 CloseWrite"的非一致场景。
func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// pump 双向泵送：mux 流 <-> TCP socket。
// 委托 pkg/iostream.Pump（C1 范本：CloseWrite 半关闭传播 + 宽限期 + 超时强制关闭；
// ForceClose 对 mux.Stream 优先 Abort，保留 P0-3 修复）。
func pump(s mux.Stream, remote net.Conn, grace time.Duration) {
	iostream.Pump(s, remote, grace)
}

// DialAllowed 限制出口模式可拨号的目标（最小授权）。
// 仅允许公网目标：IP 必须是全局单播（排除回环/私有/链路本地/多播）。
// 主机名解析后**所有**解析出的 IP 都必须是公网才放行，并返回解析后的
// IP:port 供实际拨号（防 DNS rebinding TOCTOU）。
func DialAllowed(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ipAllowed(ip) {
			return "", false
		}
		// IPv6 必须加方括号，否则 "2606::1:443" 无法被 SplitHostPort / DialTimeout 解析。
		return net.JoinHostPort(ip.String(), port), true
	}
	// 主机名：所有解析出的 IP 都必须公网才放行；返回第一个解析 IP 供拨号
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return "", false
	}
	var dialIP net.IP
	for _, ip := range ips {
		if !ipAllowed(ip) {
			return "", false
		}
		if dialIP == nil {
			dialIP = ip
		}
	}
	return net.JoinHostPort(dialIP.String(), port), true
}

// NewDialPolicy 构造出口拨号策略：默认按 DialAllowed（仅公网），
// 额外放行调用方显式指定的 CIDR 网段（如 192.168.0.0/16 允许内网服务）。
// 主机名目标解析后**所有**解析 IP 都必须公网或命中白名单才放行；
// 返回解析后的 IP:port 供实际拨号（防 DNS rebinding TOCTOU）。
func NewDialPolicy(allowCIDRs []string) func(string) (string, bool) {
	nets := make([]*net.IPNet, 0, len(allowCIDRs))
	for _, c := range allowCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		} else {
			// 非法 CIDR 静默丢弃会导致对应网段被拒，排障困难——显式告警。
			slog.Warn("忽略非法 CIDR", "cidr", c, "error", err)
		}
	}
	return func(addr string) (string, bool) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || port == "" {
			return "", false
		}
		ips := []net.IP{}
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		} else if resolved, rerr := net.LookupIP(host); rerr == nil {
			ips = append(ips, resolved...)
		}
		if len(ips) == 0 {
			return "", false
		}
		var dialIP net.IP
		for _, ip := range ips {
			allowed := ipAllowed(ip) // 公网放行
			if !allowed {
				for _, n := range nets {
					if n.Contains(ip) {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				return "", false // 任一 IP 不允许则整体拒绝
			}
			if dialIP == nil {
				dialIP = ip
			}
		}
		return net.JoinHostPort(dialIP.String(), port), true
	}
}

// NewServiceDialPolicy 构造出口拨号策略：先对节点自身宣告的服务地址做
// 精确字符串匹配放行，否则回落 NewDialPolicy(allowCIDRs) 的既有逻辑
// （公网 + 白名单 CIDR）。
//
// 命中宣告地址时：
//   - 纯 IP 宣告 → 返回同一 IP:port（IPv6 自动补方括号）；
//   - 主机名宣告 → 解析一次并返回解析后的 IP:port（消除拨号时二次解析的
//     DNS rebinding TOCTOU）。
//
// 最小授权：仅精确放行操作者显式宣告的 host:port 对；未宣告的 loopback/
// 私有地址仍被拒绝。dial 帧地址与宣告地址完全一致时（mesh connect 用
// MeshServices 返回的 addr 原样拨号）命中；其他拼写（如 localhost vs
// 127.0.0.1）不会命中，避免绕过白名单。
func NewServiceDialPolicy(allowCIDRs, serviceAddrs []string) func(string) (string, bool) {
	base := NewDialPolicy(allowCIDRs)
	exact := make(map[string]struct{}, len(serviceAddrs))
	for _, a := range serviceAddrs {
		exact[a] = struct{}{}
	}
	return func(addr string) (string, bool) {
		if _, ok := exact[addr]; ok {
			// 命中宣告地址：仍需 host:port 合法，避免拨号畸形地址。
			host, port, err := net.SplitHostPort(addr)
			if err != nil || port == "" {
				return "", false
			}
			if ip := net.ParseIP(host); ip != nil {
				// 纯 IP 宣告：原样放行（JoinHostPort 对 IPv6 补方括号）。
				return net.JoinHostPort(ip.String(), port), true
			}
			// 主机名宣告：此处解析一次并返回解析后的 IP:port（消除 Serve 在
			// 拨号时二次解析的 DNS rebinding TOCTOU）。注意：返回串不再等于
			// 原宣告地址，调用方按返回串拨号。
			ips, lerr := net.LookupIP(host)
			if lerr != nil || len(ips) == 0 {
				return "", false
			}
			return net.JoinHostPort(ips[0].String(), port), true
		}
		return base(addr)
	}
}

func ipAllowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}
