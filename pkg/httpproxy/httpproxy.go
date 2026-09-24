// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package httpproxy 提供最小可用的正向 HTTP 代理（RFC 7230/7231）：
//   - 绝对 URI 请求（GET http://host/path）→ 经注入 Dial 转发到目标
//   - CONNECT host:port（HTTPS 隧道）→ Dial 建连后 200 + 双向泵送
//   - Proxy-Authorization Basic 认证（配置了才要求）
//   - hop-by-hop 头剥离（Proxy-Authorization/Proxy-Connection/Connection 声明的字段）
//   - 防环回：转发不读系统代理环境变量，出站路由完全由注入 Dial 决定
//
// Dial 由调用方注入（sproxy mesh 场景：经 mesh 到对端出口节点，对端按 dial 帧出站拨号；
// 本地直连场景：回退 net.Dialer）。与 pkg/socks5 同模式解耦传输层，可独立单测。
package httpproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
)

// DialFunc 建立到目标 host:port 的 TCP 连接。由调用方实现传输路由。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

// Config 是 HTTP 代理配置。
type Config struct {
	// Dial 是目标拨号函数。nil 回退 net.Dialer 直连（本机出口语义）。
	// 安全边界由调用方保证：mesh 场景注入经 mesh 到显式出口节点的路由 Dial
	// （目标由出口节点 dial 策略把关），勿把本库裸 Dial 暴露给不受信客户端。
	Dial DialFunc
	// Auth 是 Proxy-Authorization Basic 校验函数；nil = 无认证。
	// 非 nil 时要求客户端提供 Basic 凭据（未认证回 407）。
	Auth func(user, pass string) bool
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
	// SelfHost 是代理自身的监听地址（host:port）。绝对 URI 请求的目标 host 等于
	// SelfHost 且路径为 /bandwidth 时**短路**返回固定带宽值（download-manager 的
	// 代理带宽探测：GET http://<proxy>/bandwidth，期望 float64 数值）。空 = 不短路。
	SelfHost string
}

// Server 是 HTTP 代理服务器（并发安全：每条连接独立 goroutine）。
type Server struct {
	cfg Config
	log *slog.Logger
}

// New 构造 HTTP 代理服务器。
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	return &Server{cfg: cfg, log: cfg.Logger}
}

// readHeaderTimeout 是单次请求头读取超时（防半开连接占用）。
const readHeaderTimeout = 30 * time.Second

// Serve 在 ln 上接受连接直到 ctx 取消或 ln 关闭（每条连接独立 goroutine）。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(c)
	}
}

// handleConn 处理一条客户端连接：循环读请求（keep-alive），绝对 URI 转发 / CONNECT 隧道。
func (s *Server) handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(readHeaderTimeout))
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				s.log.Warn("读代理请求失败", "error", err)
			}
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			if !s.handleConnect(c, req) {
				return
			}
			continue
		}
		if !s.handleForward(c, req) {
			return
		}
	}
}

// authenticate 校验 Proxy-Authorization Basic（未配置认证直接放行）。
func (s *Server) authenticate(req *http.Request) bool {
	if s.cfg.Auth == nil {
		return true
	}
	h := req.Header.Get("Proxy-Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, prefix))
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return false
	}
	return s.cfg.Auth(user, pass)
}

// writeRawError 在裸 TCP 连接上写原始 HTTP 错误响应（不含内部细节）。
// extraHeaders 用于携带 Proxy-Authenticate 等。
func writeRawError(c net.Conn, code int, text, body string, extraHeaders map[string]string) {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", code, text)
	for k, v := range extraHeaders {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	} else {
		b.WriteString("Content-Length: 0\r\n")
	}
	b.WriteString("Connection: close\r\n\r\n")
	if body != "" {
		b.WriteString(body)
	}
	_, _ = c.Write([]byte(b.String()))
}

// write407 回 407 Proxy Authentication Required。
func (s *Server) write407(c net.Conn) {
	writeRawError(c, http.StatusProxyAuthRequired, "Proxy Authentication Required", "proxy authentication required",
		map[string]string{"Proxy-Authenticate": `Basic realm="sproxy"`})
}

// hopHeaders 是需要剥离的逐跳头（转发时）。
var hopHeaders = map[string]bool{
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// handleForward 处理绝对 URI 请求（GET http://host/path）→ 经注入 Dial 转发。
func (s *Server) handleForward(c net.Conn, req *http.Request) bool {
	if !s.authenticate(req) {
		// 读空 body 后回 407（避免连接关闭时客户端收到 RST）。
		// 错误路径返回 false：writeRawError 恒写 Connection: close，
		// 保持 keep-alive 语义一致（认证失败不复用连接）。
		_, _ = io.Copy(io.Discard, req.Body)
		s.write407(c)
		return false
	}
	// 仅允许绝对 URI（RFC 7230 §5.3.2）：req.URL 必须带 scheme+host。
	if !req.URL.IsAbs() || req.URL.Host == "" {
		writeRawError(c, http.StatusBadRequest, "Bad Request", "bad request", nil)
		return false
	}
	// download-manager 带宽探测短路：GET http://<代理自身>/bandwidth（绝对 URI host
	// 等于 SelfHost）。dm 的 getProxyBandwidth 期望 body 是 float64 数值（ParseFloat），
	// 数值大好 = 带宽好——返回固定 "100.0" 表示代理可用。不误伤：仅路径 /bandwidth
	// 且目标 host 严格等于 SelfHost 才短路。
	if s.cfg.SelfHost != "" && req.URL.Host == s.cfg.SelfHost && req.URL.Path == "/bandwidth" && req.Method == http.MethodGet {
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\nConnection: close\r\n\r\n100.0")
		return false
	}
	// 转发：经注入 Dial 建连后，把请求原样写到目标连接。
	dialCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, err := s.cfg.Dial(dialCtx, req.URL.Host)
	if err != nil {
		s.log.Warn("转发拨号失败", "addr", req.URL.Host, "error", err)
		writeRawError(c, http.StatusBadGateway, "Bad Gateway", "bad gateway", nil)
		return false
	}
	defer upstream.Close()
	// 写请求行 + 头（剥离 hop-by-hop；host 保留）
	req.Host = req.URL.Host
	stripHopHeaders(req.Header)
	if werr := req.Write(upstream); werr != nil {
		return false
	}
	// 读响应回写客户端
	resp, rerr := http.ReadResponse(bufio.NewReader(upstream), req)
	if rerr != nil {
		s.log.Warn("读上游响应失败", "error", rerr)
		return false
	}
	defer resp.Body.Close()
	// 回写响应（含状态行/头/body 流式）
	if werr := resp.Write(c); werr != nil {
		return false
	}
	return true
}

// handleConnect 处理 CONNECT host:port（HTTPS 隧道）→ Dial 建连 + 200 + 双向泵送。
func (s *Server) handleConnect(c net.Conn, req *http.Request) bool {
	if !s.authenticate(req) {
		s.write407(c)
		return false
	}
	target := req.Host
	if target == "" {
		target = req.URL.Host
	}
	if target == "" {
		writeRawError(c, http.StatusBadRequest, "Bad Request", "bad request", nil)
		return false
	}
	dialCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, err := s.cfg.Dial(dialCtx, target)
	if err != nil {
		s.log.Warn("CONNECT 拨号失败", "addr", target, "error", err)
		writeRawError(c, http.StatusBadGateway, "Bad Gateway", "bad gateway", nil)
		return false
	}
	defer upstream.Close()
	if _, err := fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return false
	}
	// 双向泵送（半关闭 + grace，对齐 iostream.Pump 范本）。
	iostream.Pump(c, upstream, iostream.PumpGrace)
	return false // 隧道结束后连接不再复用
}

// stripHopHeaders 剥离逐跳头（含 Connection 声明的字段）。
func stripHopHeaders(h http.Header) {
	for _, key := range h.Values("Connection") {
		for f := range strings.SplitSeq(key, ",") {
			h.Del(strings.TrimSpace(f))
		}
	}
	for key := range hopHeaders {
		h.Del(key)
	}
}
