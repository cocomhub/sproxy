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

// Serve 是叶子侧的流接收循环。
// localAddr 是本地 HTTP 服务地址（HTTP 中继转发目标）；
// dialAllow 为 true 时启用出口模式（收到 dial 帧可出站连接）。
func Serve(ctx context.Context, m *mux.Mux, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
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
				if !DialAllowed(d.Dial) {
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
	<-done
	<-done
}

// DialAllowed 限制出口模式可拨号的目标（最小授权）。
func DialAllowed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 主机名：允许（公网域名也放行，便于访问云服务器）
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
