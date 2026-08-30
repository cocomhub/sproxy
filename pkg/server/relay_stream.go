// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// relayStreamDialResultTimeout 是 hub 等待叶子拨号结果帧的超时（I27）。
// 取略大于叶子 net.DialTimeout（10s）的值，避免「慢但成功」的拨号被误判 504；
// 叶子崩溃/旧叶子不回帧时，超时后回 504。
const relayStreamDialResultTimeout = 12 * time.Second

// relayStreamGrace 是中继流泵送一方向完成后的半关闭宽限期（P0-4）。
// 与叶子/客户端 pump 的 pumpGracePeriod（60s）对齐：宽限期内另一方向应完成
// 半关闭收尾（在途响应读回）；超时视为对端非合作，强制关闭释放，防
// goroutine/FD 泄漏。
const relayStreamGrace = 60 * time.Second

// relayStreamIdleTimeout 是中继流 200 后无任何数据流量的空闲超时（P1-9）。
// 取 15 分钟：远大于正常交互/SSH keepalive 间隔，同时防止"拿到 200 后不发"的
// 客户端无限期占用叶子出站 FD 与 mux 流。
const relayStreamIdleTimeout = 15 * time.Minute

// maxDialResultFrameBytes 是拨号结果帧（[4B len][JSON]）的 JSON 部分长度上限。
const maxDialResultFrameBytes = 4096

// RelayStreamRequest 是任意 TCP 流中继请求（对应 hub.DialRequest）。
type RelayStreamRequest struct {
	Target string `json:"target"`
	Type   string `json:"type"` // 固定 "tcp"
	Addr   string `json:"addr"` // 目标叶子要出站连接的 TCP 地址（如 target-host:22）
}

// RelayStreamHandler 通过 hub 路由表把一条 HTTP 请求升级为到目标叶子的双向字节流，
// 实现任意 TCP（SSH/长连接）中继。
type RelayStreamHandler struct {
	routeTable *hub.MeshRouteTable
	logger     *slog.Logger
	// idleTimeout 是中继流 200 后无任何数据流量的空闲超时（P1-9）；0 表示不启用。
	// 防"拿到 200 后不发"的客户端无限期占用叶子出站 FD 与 mux 流。
	idleTimeout time.Duration
}

// NewRelayStreamHandler 创建流中继处理器。
func NewRelayStreamHandler(rt *hub.MeshRouteTable, logger *slog.Logger) *RelayStreamHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &RelayStreamHandler{routeTable: rt, logger: logger, idleTimeout: relayStreamIdleTimeout}
}

// validateRelayAddr 对中继目标地址做语法级校验（I26）：必须是 host:port 格式，
// 端口为 1-65535 数值，且不含 @ / 协议分隔符 / 控制字符。
// 仅做输入卫生（fail-fast，开流前拦截畸形输入，省 mux 流与叶子资源），
// 不限制回环/私网——叶子侧 DialPolicy 是最终安全边界。
func validateRelayAddr(addr string) error {
	if addr == "" {
		return errors.New("addr 为空")
	}
	if strings.Contains(addr, "@") {
		return errors.New("addr 不能包含 @")
	}
	if strings.Contains(addr, "://") {
		return errors.New("addr 不能包含协议分隔符")
	}
	for i := 0; i < len(addr); i++ {
		if c := addr[i]; c < 0x20 || c == 0x7f {
			return errors.New("addr 包含控制字符")
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return errors.New("addr 必须是 host:port 格式")
	}
	p, perr := strconv.ParseUint(port, 10, 16)
	if perr != nil || p == 0 {
		return errors.New("addr 端口必须是 1-65535 的数值")
	}
	return nil
}

// writeRelayDialFrame 在 mux 流上写入 [4B len][JSON] dial 帧，处理部分写（S37）：
// mux 流在发送窗口小于帧长时返回 n<len 的短写，n<len 且 err==nil 视为错误。
func writeRelayDialFrame(stream mux.Stream, head []byte) error {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	for _, b := range [][]byte{lenBuf, head} {
		n, err := stream.Write(b)
		if err != nil {
			return err
		}
		if n != len(b) {
			return fmt.Errorf("短写: wrote %d of %d 字节", n, len(b))
		}
	}
	return nil
}

// readDialResultFrame 从 mux 流读取叶子拨号结果帧 [4B len][DialResultFrame JSON]（I27）。
// 必须在 goroutine 中调用（mux.Stream 无 deadline），由调用方 select 超时/取消。
func readDialResultFrame(stream mux.Stream, result *hub.DialResultFrame) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return err
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen == 0 || metaLen > maxDialResultFrameBytes {
		return fmt.Errorf("非法结果帧长度: %d", metaLen)
	}
	meta := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, meta); err != nil {
		return err
	}
	return json.Unmarshal(meta, result)
}

// ServeHTTP 处理中继流请求：
//  1. 解析 RelayStreamRequest（含 addr 语法校验 I26 / routeTable nil 守卫 S34）
//  2. 查找目标叶子 mux
//  3. 在目标 mux 上 Open 一条流，写入 DialRequest 首帧（短写防护 S37）
//  4. 带超时读叶子拨号结果帧（I27）：ok → 升级原始 TCP 并回 200；error/EOF → 502；
//     超时 → 504
//  5. 双向 io.Copy（收尾用 Abort 非阻塞关闭 I28）
//
// 叶子侧（sclient relay start / portal）在收到 DialRequest 后向 addr 发起出站
// net.Dial，随后把远程流与本地 socket 双向泵送。仅 relay start 模式回结果帧；
// webrtc 直连（p2p listen）不回帧，因此经 webrtc 直连的 hub 读帧会超时 504——
// 该路径本就不经本 handler（mesh webrtc 直连由客户端直接写 dial 帧）。
func (h *RelayStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// P1-10：用 MaxBytesReader 而非 io.LimitReader——后者把超大请求体静默截断，
	// 前 1MiB 内合法 JSON 照样 200 且开始真实拨号（调用方依赖 400/413 拒绝会失察）。
	// MaxBytesReader 超限时 json.Decode 报 *http.MaxBytesError → 413（对齐
	// signaling_handlers.go 的 S41 语义）。
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req RelayStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "中继请求体过大", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if req.Target == "" || req.Addr == "" {
		http.Error(w, "缺少 target 或 addr", http.StatusBadRequest)
		return
	}
	if req.Type != "tcp" {
		http.Error(w, fmt.Sprintf("不支持的 type: %q", req.Type), http.StatusBadRequest)
		return
	}
	// I26：开流前做 addr 语法校验（fail-fast），避免畸形地址白占 mux 流与叶子资源。
	if err := validateRelayAddr(req.Addr); err != nil {
		http.Error(w, fmt.Sprintf("非法 addr: %v", err), http.StatusBadRequest)
		return
	}
	// S34：routeTable nil 防护（与 hubNodesHandler 等守卫一致）。
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}

	targetMux := h.routeTable.Lookup(hub.NodeID(req.Target))
	if targetMux == nil {
		h.logger.Warn("流中继目标节点未找到", "target", req.Target)
		http.Error(w, fmt.Sprintf("目标节点 %s 未找到", req.Target), http.StatusNotFound)
		return
	}
	// M-9 路由面隔离：目标节点必须与调用方同 mesh，跨 mesh 目标对外不可见（404 防节点存在性探测）。
	if targetInfo, ok := h.routeTable.LookupInfo(hub.NodeID(req.Target)); ok && targetInfo.Mesh != meshFromRequest(r) {
		h.logger.Warn("流中继目标节点跨 mesh，拒绝", "target", req.Target, "mesh", targetInfo.Mesh)
		http.Error(w, fmt.Sprintf("目标节点 %s 未找到", req.Target), http.StatusNotFound)
		return
	}

	// 在目标 mux 上打开一条流，写入「隧道元数据帧」格式的 dial 指令：
	//   [4B big-endian length][JSON {"dial":"addr"}]
	// 叶子侧（自定义 accept 循环）读到该帧后向 addr 发起出站 TCP。
	// Open 受 relayStreamDialResultTimeout 约束：目标叶子若「已注册但不读」其底层
	// 传输写阻塞（TCP 传输 Send 同步写、持锁最长 60s），Open 也能在有限时间内失败
	// 回 502，而非静默挂起（WS 的 Send 异步入队不受此影响，超时仅兜底）。
	openCtx, openCancel := context.WithTimeout(r.Context(), relayStreamDialResultTimeout)
	defer openCancel()
	stream, err := targetMux.Open(openCtx)
	if err != nil {
		h.logger.Error("打开流中继流失败", "target", req.Target, "error", err)
		http.Error(w, fmt.Sprintf("打开流失败: %v", err), http.StatusBadGateway)
		return
	}
	// 保留 Close() 以便提前失败路径向叶子发 FrameClose 通知；收尾/超时路径显式
	// 调 Abort() 后，此处的 Close() 因 done 已关闭而立即返回（非阻塞）。
	defer stream.Close()

	head, merr := json.Marshal(hub.DialRequest{Dial: req.Addr})
	if merr != nil {
		http.Error(w, fmt.Sprintf("序列化 dial 指令失败: %v", merr), http.StatusInternalServerError)
		return
	}
	if werr := writeRelayDialFrame(stream, head); werr != nil {
		h.logger.Error("写 dial 指令失败", "target", req.Target, "error", werr)
		http.Error(w, fmt.Sprintf("写 dial 指令失败: %v", werr), http.StatusBadGateway)
		return
	}

	// I27：写 200 前带超时读叶子拨号结果帧，使 200 语义变为「数据面就绪」。
	// 叶子以 DialResultFrames 模式（relay start）运行时回帧；旧叶子/webrtc 直连
	// 不回帧 → 超时后 504。读帧 goroutine 在超时/取消路径经 stream.Abort() 回收
	// （mux.Stream 无 deadline，Abort 关闭 dataCh/done 立即解除 Read 阻塞）。
	dialResultCh := make(chan hub.DialResultFrame, 1)
	readErrCh := make(chan error, 1)
	go func() {
		var result hub.DialResultFrame
		if rerr := readDialResultFrame(stream, &result); rerr != nil {
			readErrCh <- rerr
			return
		}
		dialResultCh <- result
	}()

	select {
	case result := <-dialResultCh:
		if result.DialResult != hub.DialResultOK {
			_ = stream.Abort()
			h.logger.Warn("叶子拨号失败", "target", req.Target, "addr", req.Addr, "message", result.Message)
			http.Error(w, fmt.Sprintf("目标拨号失败: %s", result.Message), http.StatusBadGateway)
			return
		}
	case rerr := <-readErrCh:
		_ = stream.Abort()
		h.logger.Warn("读叶子拨号结果失败", "target", req.Target, "error", rerr)
		http.Error(w, fmt.Sprintf("读取拨号结果失败: %v", rerr), http.StatusBadGateway)
		return
	case <-time.After(relayStreamDialResultTimeout):
		_ = stream.Abort()
		h.logger.Warn("等待叶子拨号结果超时", "target", req.Target, "addr", req.Addr)
		http.Error(w, "等待叶子拨号结果超时", http.StatusGatewayTimeout)
		return
	case <-r.Context().Done():
		_ = stream.Abort()
		h.logger.Warn("中继请求已取消", "target", req.Target)
		return
	}

	// 升级为原始 TCP 流，做双向泵送
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "服务器不支持连接升级", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("升级连接失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	// Hijack 后 http.ResponseWriter 不再可用；写首行表示连接建立
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	// 双向泵送：浏览器/SSH 端 <-> mux 流 <-> 叶子出站 TCP
	// 读侧必须用 rw.Reader（包含 conn + 已缓冲字节），否则 Hijack 时
	// 客户端在请求体后追加的数据若已被 bufio 预读，从 conn 直接读会跳过
	// 缓冲字节导致流错位（I4'）。写侧用 conn（rw.Writer 内部就是 conn）。
	//
	// P1-9 空闲超时：任一方向有字节流动即刷新 lastActive；watchdog 在 idleTimeout
	// 内无任何数据则强制关闭两端，防"拿到 200 后不发"的客户端无限期占用叶子 FD。
	var lastActive atomic.Int64
	lastActive.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, &activityReader{r: rw, lastActive: &lastActive})
		_ = stream.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, &activityReader{r: stream, lastActive: &lastActive})
		// 半关闭客户端写侧（TCP FIN），使客户端感知远端 EOF。conn 为 Hijack 后的
		// 原始连接（*net.TCPConn），支持 CloseWrite；其他类型退化 Close。
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = conn.Close()
		}
		done <- struct{}{}
	}()

	// P1-9 watchdog：空闲超时强制关闭（Abort 非阻塞，解除两个 io.Copy 的阻塞，
	// 随后的 done 收尾让泵送函数正常返回）。idleTimeout<=0 时不启用。
	if h.idleTimeout > 0 {
		watchdogDone := make(chan struct{})
		defer close(watchdogDone)
		go func() {
			ticker := time.NewTicker(h.idleTimeout / 2)
			defer ticker.Stop()
			for {
				select {
				case <-watchdogDone:
					return
				case now := <-ticker.C:
					if now.Sub(time.Unix(0, lastActive.Load())) > h.idleTimeout {
						_ = conn.Close()
						_ = stream.Abort()
						return
					}
				}
			}
		}()
	}

	// 半关闭宽限期（P0-4，对齐 leaf.go pump 的 C1 范本）：任一方向完成后，
	// 给另一方向 relayStreamGrace 时间完成半关闭收尾——让在途响应仍可被读回
	// （write-then-read 流不截断）。超时视为对端非合作，强制关闭释放。
	// 注意收尾路径统一用非阻塞 Abort()（writeCh 打满时阻塞版 Close 会永久挂起，
	// I28），conn.Close() 亦非阻塞。
	remaining := 2
	var timeoutCh <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for remaining > 0 {
		select {
		case <-done:
			remaining--
			if remaining == 1 {
				// 一个方向完成：武装宽限期等待另一半完成半关闭收尾。
				timer = time.NewTimer(relayStreamGrace)
				timeoutCh = timer.C
			}
		case <-timeoutCh:
			// 非合作对端：强制关闭两端，解除 io.Copy 对 conn/stream 的阻塞。
			_ = conn.Close()
			_ = stream.Abort()
			for remaining > 0 { // 关闭后 Read/Write 立即返回，等待 goroutine 退出
				<-done
				remaining--
			}
			return
		}
	}
}

// activityReader 包装 io.Reader，Read 读到数据时刷新 lastActive（P1-9 空闲超时判定）。
// 中继泵送两个方向的读源（客户端 rw 与叶子 stream）都经此包装，任一方向有字节
// 流动即视为连接活跃。
type activityReader struct {
	r          io.Reader
	lastActive *atomic.Int64
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.lastActive.Store(time.Now().UnixNano())
	}
	return n, err
}
