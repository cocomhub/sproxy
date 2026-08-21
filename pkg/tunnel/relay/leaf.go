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
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// ServeOptions 配置 Serve 的拨号策略。
type ServeOptions struct {
	// DialPolicy 出口模式下的目标地址校验 + 解析策略。
	// 返回 (resolvedAddr, ok)：ok 表示放行，resolvedAddr 是实际应拨的地址
	// （主机名会解析为具体 IP，防 DNS rebinding TOCTOU）。
	// nil 时使用 DialAllowed（严格：仅允许公网目标）。
	DialPolicy func(addr string) (resolvedAddr string, ok bool)
}

// Serve 是叶子侧的流接收循环。
// localAddr 是本地 HTTP 服务地址（HTTP 中继转发目标）；
// dialAllow 为 true 时启用出口模式（收到 dial 帧可出站连接）。
func Serve(ctx context.Context, m *mux.Mux, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger, opts ...ServeOptions) error {
	if logger == nil {
		logger = slog.Default()
	}
	dialPolicy := DialAllowed
	for _, o := range opts {
		if o.DialPolicy != nil {
			dialPolicy = o.DialPolicy
		}
	}
	_ = dialPolicy
	for {
		stream, err := m.Accept(ctx)
		if err != nil {
			return err
		}
		go func(s mux.Stream) {
			defer s.Close()

			lenBuf := make([]byte, 4)
			if _, rerr := io.ReadFull(s, lenBuf); rerr != nil {
				return
			}
			metaLen := binary.BigEndian.Uint32(lenBuf)
			if metaLen == 0 || metaLen > tunnel.MaxMetadataBytes {
				return
			}
			meta := make([]byte, metaLen)
			if _, rerr := io.ReadFull(s, meta); rerr != nil {
				return
			}

			// 先按 dial 帧解析
			var d hub.DialRequest
			if err := json.Unmarshal(meta, &d); err == nil && d.Dial != "" {
				if !dialAllow {
					logger.Warn("收到 dial 帧但未开启 --dial-allow", "addr", d.Dial)
					return
				}
				// 策略返回实际应拨的地址（已解析 IP，防 DNS rebinding TOCTOU）
				resolved, ok := dialPolicy(d.Dial)
				if !ok {
					logger.Warn("出口模式收到非法 dial 地址", "addr", d.Dial)
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
					return
				}
				defer remote.Close()
				pump(s, remote)
				return
			}

			// 否则按隧道 HTTP 中继处理
			var req tunnel.Request
			if err := json.Unmarshal(meta, &req); err != nil || req.Method == "" {
				logger.Warn("无法解析的中继帧")
				return
			}
			serveHTTP(ctx, s, localAddr, req, httpClient, logger)
		}(stream)
	}
}

// serveHTTP 处理隧道 HTTP 中继流（metadata 已解析）。
func serveHTTP(ctx context.Context, s mux.Stream, localAddr string, req tunnel.Request, httpClient *http.Client, logger *slog.Logger) {
	forwardURL := localAddr + req.URL
	// body 处理：仅对允许带 body 的方法把流 s 作为请求体（NopCloser 避免
	// http.Client 读完 body 后 Close(s) 关掉整条 mux 流）。GET/HEAD/DELETE 等
	// 无 body 方法不设 body，否则 http.Client 会尝试读 s 到 EOF 干扰协议。
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

// methodAllowsBody 报告 HTTP 方法是否允许携带请求体。
// 仅对允许 body 的方法把隧道流作为请求体；GET/HEAD 等无 body 方法不设 body，
// 否则 http.Client 会尝试读流到 EOF，干扰后续响应写入。
func methodAllowsBody(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

// pump 双向泵送：mux 流 <-> TCP socket。
func pump(s mux.Stream, remote net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, s)
		if tc, ok := remote.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			_ = remote.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(s, remote)
		_ = s.CloseWrite()
		done <- struct{}{}
	}()
	// 一个方向完成即解除另一方向阻塞，防非合作 TCP 永久挂起
	<-done
	_ = s.Close()
	<-done
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
		return ip.String() + ":" + port, true
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

func ipAllowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}
