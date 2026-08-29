// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// relayHandshakeTimeout 是 RelayStream 握手阶段（写请求 + 读状态行/响应头）的
// 默认读写超时。必须大于服务端 relayStreamDialResultTimeout（12s），避免
// 「叶子慢但合法」的拨号（hub 等到 12s 决策后才回 200）被客户端提前放弃，
// 导致 MeshConnect 错误回退。
const relayHandshakeTimeout = 30 * time.Second

// RelayStreamRequest 是向 hub 发起任意 TCP 流中继的请求体。
type RelayStreamRequest struct {
	Target string `json:"target"`
	Type   string `json:"type"` // 固定 "tcp"
	Addr   string `json:"addr"` // 目标叶子要出站连接的 TCP 地址
}

// RelayStream 通过 hub 的 /api/relay/stream 建立一条到目标叶子出口节点的
// 双向字节流。返回的 net.Conn 代表「本地 ↔ hub ↔ 叶子出站 TCP」全链路，
// 调用方拿到后按普通 socket 使用（如 SSH 客户端连接）。
//
// 该连接不经过 http.Client 的请求/响应模型，而是直接拨 hub 地址发原始
// HTTP POST（CONNECT 风格），成功建立后返回底层连接。目标叶子必须开启
// --dial-allow（出口模式）才能出站 dial 指定 addr。
//
// 鉴权与传输注意（I36）：
//   - 本方法始终直接拨 c.serverURL（raw HTTP CONNECT 风格），不经过
//     WithTunnel/WithXfer 配置的隧道/xfer 传输。当服务端配置了 auth_token 或
//     api_keys 时，必须用 WithAuthToken 配置同一凭据，否则直连返回 401。
//   - 隧道/xfer 模式下 MeshServices（/api/hub/services）走 localMux 无需 Bearer，
//     而本方法（/api/relay/stream）仅注册在 srvMux + authMiddleware——两条路径
//     鉴权要求不同，配置隧道时请一并配置 auth_token。
//   - 握手阶段有界（I33）：写请求/读状态行与响应头受 min(ctx deadline, 30s)
//     deadline 与 ctx 取消 watchdog 保护；握手完成后清除 deadline，长连接数据面
//     不受影响。
func (c *FileClient) RelayStream(ctx context.Context, target, addr string) (net.Conn, error) {
	if target == "" || addr == "" {
		return nil, fmt.Errorf("RelayStream: target 与 addr 均不能为空")
	}
	body, err := json.Marshal(RelayStreamRequest{Target: target, Type: "tcp", Addr: addr})
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 序列化请求失败: %w", err)
	}

	u, err := url.Parse(c.serverURL)
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 解析 serverURL 失败: %w", err)
	}
	scheme := u.Scheme
	host := u.Host
	if host == "" {
		return nil, fmt.Errorf("RelayStream: 无效的 serverURL %q", c.serverURL)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var raw net.Conn
	switch scheme {
	case "https", "wss":
		// 用 tls.Dialer.DialContext（支持 ctx 取消，Ctrl+C 可中断 TLS 拨号）。
		// 注：wss/ws 在此仅表示「是否 TLS」，不引入 WebSocket 升级语义（S47 容错）。
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: c.relayTLSConfig()}
		raw, err = tlsDialer.DialContext(ctx, "tcp", host)
	case "http", "ws":
		raw, err = dialer.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("RelayStream: 不支持的 scheme %q", scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 连接 hub 失败: %w", err)
	}

	// I33：握手阶段有界——deadline 取 min(ctx deadline, 30s)；ctx-watchdog 在
	// ctx 取消时立即关闭底层连接，解除对端半开（hub 进程卡死/链路黑洞）导致的
	// 无限阻塞。握手完成后清除 deadline 并停止 watchdog，长连接数据面不受影响。
	handshakeDeadline := time.Now().Add(relayHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(handshakeDeadline) {
		handshakeDeadline = dl
	}
	if err = raw.SetDeadline(handshakeDeadline); err != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 设置握手 deadline 失败: %w", err)
	}
	stopWatchdog := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-stopWatchdog:
		}
	}()
	// cleanup 幂等：停止 watchdog 并等待其退出（成功/失败路径统一收尾，防泄漏）。
	cleanup := func() {
		select {
		case <-stopWatchdog:
		default:
			close(stopWatchdog)
		}
		<-watchdogDone
	}
	defer cleanup()

	// 发送原始 HTTP CONNECT 风格请求
	path := "/api/relay/stream"
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	if c.authToken != "" {
		fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", c.authToken)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, werr := io.WriteString(raw, b.String()); werr != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 写请求头失败: %w", werr)
	}
	if _, werr := raw.Write(body); werr != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 写请求体失败: %w", werr)
	}

	// 读取响应头，校验是否成功建立
	br := bufio.NewReader(raw)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 读响应状态失败: %w", err)
	}
	// S45：解析状态码而非脆弱的 Contains(" 200 ")。不要用 http.ReadResponse——
	// CONNECT 200 后紧跟数据面字节，ReadResponse 会把数据当 body 消费，破坏流。
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	statusCode := ""
	if len(parts) >= 2 {
		statusCode = parts[1]
	}
	if statusCode != "200" {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		raw.Close()
		if statusCode == "401" {
			// I36：401 诊断错误——隧道/xfer 模式经 localMux 访问 /api/hub/services
			// 成功，但本方法直拨 /api/relay/stream 仅 srvMux + Bearer。
			return nil, fmt.Errorf("RelayStream: hub 返回 401 未授权（可能原因：隧道/xfer 模式 + 服务端强制 Bearer + 客户端未配 auth_token，请用 WithAuthToken 配置与 auth_token 一致的凭据）")
		}
		return nil, fmt.Errorf("RelayStream: hub 返回 %s%s", strings.TrimSpace(statusLine), string(rest))
	}
	// 读取剩余响应头直到空行
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			raw.Close()
			return nil, fmt.Errorf("RelayStream: 读响应头失败: %w", rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 清除握手 deadline：长连接数据面（SSH 等）不受残留 deadline 影响
	_ = raw.SetDeadline(time.Time{})

	// 返回原始连接（bufio.Reader 中可能已缓冲后续数据，包装回 raw）
	return &bufferedNetConn{Conn: raw, reader: br}, nil
}

// relayTLSConfig 返回用于连接 hub 的 TLS 配置，兼容自签证书。
// 优先沿用 http.Client Transport 上的 TLSClientConfig（WithInsecureTLS/WithClientCert 设置）。
func (c *FileClient) relayTLSConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if u, err := url.Parse(c.serverURL); err == nil && u.Hostname() != "" {
		cfg.ServerName = u.Hostname()
	}
	// 从 httpClient 的 Transport 继承 TLS 配置（自签/客户端证书/RootCAs 等）
	if c.httpClient != nil {
		if tr, ok := c.httpClient.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			cfg.InsecureSkipVerify = tr.TLSClientConfig.InsecureSkipVerify
			if len(tr.TLSClientConfig.Certificates) > 0 {
				cfg.Certificates = tr.TLSClientConfig.Certificates
			}
			// I34：继承 RootCAs，私有 CA 签发的 hub 证书在中继拨号时可正常校验。
			// *x509.CertPool 只读共享安全；nil 时保持 nil（走系统根，行为不变）。
			if tr.TLSClientConfig.RootCAs != nil {
				cfg.RootCAs = tr.TLSClientConfig.RootCAs
			}
		}
	}
	return cfg
}

// bufferedNetConn 包装 net.Conn，使 bufio.Reader 中已缓冲的数据可被继续读取。
//
// 并发约束（S46）：不支持并发读——bufio.Reader 非并发安全，调用方必须保证同一
// 时刻只有一个 goroutine 在 Read。SSH 等半关闭协议可调用 CloseWrite 关闭写方向。
type bufferedNetConn struct {
	net.Conn
	reader *bufio.Reader
}

func (b *bufferedNetConn) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

// CloseWrite 透传到底层连接（如 *net.TCPConn / *tls.Conn），支持 SSH 等半关闭
// 协议。底层不支持 CloseWrite 时返回错误而非 panic（防御而非崩溃）。
func (b *bufferedNetConn) CloseWrite() error {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := b.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return fmt.Errorf("bufferedNetConn: 底层连接 %T 不支持 CloseWrite", b.Conn)
}

// MeshService 是 hub 返回的一条 mesh 服务宣告。
type MeshService struct {
	Name string `json:"name"`
	Node string `json:"node"`
	Addr string `json:"addr,omitempty"`
}

// MeshServices 查询 hub 上所有节点宣告的 mesh 服务（供选路发现）。
func (c *FileClient) MeshServices(ctx context.Context) ([]MeshService, error) {
	var svcs []MeshService
	if err := c.doJSON(ctx, http.MethodGet, "/api/hub/services", nil, &svcs); err != nil {
		return nil, fmt.Errorf("查询 mesh 服务失败: %w", err)
	}
	return svcs, nil
}

// MeshConnect 查找托管指定服务的节点并建立经 hub 的流中继连接。
// 返回的 net.Conn 代表「本地 ⇄ hub ⇄ 托管节点（出口或本地服务）」。
// 目标节点必须已宣告该服务（relay start/portal 通过 Meta.Services 宣告）。
// 若多个节点宣告同名服务，依次尝试直到某个建立成功（首个离线不影响其余）。
//
// 注意：
//   - relay-only（S48）：本方法仅经 hub 中继（/api/relay/stream），不含 WebRTC
//     直连。CLI 的 mesh connect 是 webrtc 优先 + 中继回落的超集，需要直连请用 CLI。
//   - 数据面就绪语义（I35/B4）：hub 在叶子拨号成功（ok 帧）后才回 200；叶子拨号
//     失败/超时会回 502/504，本方法据此回退下一候选。固有限制：若叶子拨号成功但
//     远端连接建立后立即断开（200 + 立即 EOF），本方法会返回一个已死的连接且不
//     触发回退——CONNECT 风格协议无法在不破坏通用性（任意 TCP、无应用层 echo）
//     的前提下探测。
//   - 鉴权（I36）：本方法依赖 RelayStream 直拨 serverURL，服务端配置 auth_token /
//     api_keys 时须用 WithAuthToken 配置同一凭据。
func (c *FileClient) MeshConnect(ctx context.Context, service string) (net.Conn, string, error) {
	svcs, err := c.MeshServices(ctx)
	if err != nil {
		return nil, "", err
	}
	var lastErr error
	for _, s := range svcs {
		if s.Name != service {
			continue
		}
		conn, cerr := c.RelayStream(ctx, s.Node, s.Addr)
		if cerr != nil {
			lastErr = cerr
			continue // 该节点不可达，尝试下一个候选
		}
		return conn, s.Node, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("mesh 服务 %q 的所有候选节点均连接失败: %w", service, lastErr)
	}
	return nil, "", fmt.Errorf("mesh 服务 %q 未找到（请确认目标节点已宣告该服务）", service)
}
