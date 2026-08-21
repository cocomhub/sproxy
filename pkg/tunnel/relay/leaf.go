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
	// DialPolicy 出口模式下的目标地址校验策略。
	// nil 时使用 DialAllowed（严格：仅允许公网目标）。
	DialPolicy func(addr string) bool
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
				if !dialPolicy(d.Dial) {
					logger.Warn("出口模式收到非法 dial 地址", "addr", d.Dial)
					return
				}
				logger.Info("出口拨号", "addr", d.Dial)
				remote, derr := net.DialTimeout("tcp", d.Dial, 10*time.Second)
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
	localReq, err := http.NewRequestWithContext(ctx, req.Method, forwardURL, s) //nolint:gosec // G704: SSRF is intentional (relay proxy)
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
// 仅允许公网目标：IP 必须是全局单播（排除回环/私有/链路本地/多播），
// 主机名解析后同样按解析出的 IP 校验（防 DNS rebinding 指向内网）。
func DialAllowed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipAllowed(ip)
	}
	// 主机名：解析后校验所有解析出的 IP，全部公网才放行
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ipAllowed(ip) {
			return false
		}
	}
	return true
}

// NewDialPolicy 构造出口拨号策略：默认按 DialAllowed（仅公网），
// 额外放行调用方显式指定的 CIDR 网段（如 192.168.0.0/16 允许内网服务）。
// 主机名目标解析后按解析出的 IP 判定是否命中白名单。
func NewDialPolicy(allowCIDRs []string) func(string) bool {
	nets := make([]*net.IPNet, 0, len(allowCIDRs))
	for _, c := range allowCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return func(addr string) bool {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return false
		}
		ips := []net.IP{}
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		} else if resolved, rerr := net.LookupIP(host); rerr == nil {
			ips = append(ips, resolved...)
		}
		for _, ip := range ips {
			if ipAllowed(ip) {
				return true // 公网目标放行
			}
			for _, n := range nets {
				if n.Contains(ip) {
					return true // 命中显式白名单网段
				}
			}
		}
		return false
	}
}

func ipAllowed(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}
