// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// 跨 hub 中继转发（hub 联邦 B）：本 hub 路由表未命中目标节点时，把 relay 拨号
// 请求转发到「上报该节点的联邦对端 hub」，实现 A→hub1→hub2→B 链式中继。
//
// 复用对端 /api/relay/stream（SproxySig 认证 + CONNECT 风格 + mux 拨号帧），
// 不发明新协议；转发链每个环节都走既有认证（不能因跨 hub 跳过准入）。
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// 跨 hub 转发防环元数据头（hop-by-hop，转发时附加；SproxySig 只签名
// method/path/query/body，头不参与签名——此处是转发链内部控制面）。
const (
	relayForwardHopHeader  = "X-Relay-Hop"
	relayForwardPathHeader = "X-Relay-Path"

	// defaultRelayMaxHops 是跨 hub 转发链的最大跳数（防环硬上限）。
	// 链式路径 A→hub1→hub2→B 是 1 跳转发；上限 4 允许 5 级 hub 链，同时任何
	// 环路在 4 跳后被截断（不无限循环）。合理链式深度远小于此值。
	defaultRelayMaxHops = 4

	// relayForwardHandshakeTimeout 是跨 hub 转发 CONNECT 握手的读写超时。
	// 必须大于对端 hub 的 relayStreamDialResultTimeout（12s）+ 处理余量，避免
	// 「叶子慢但合法」的拨号被本端提前放弃；对端崩溃/挂起时超时后以明确错误
	// 传播（转发链各环节超时有界，不静默挂起）。
	relayForwardHandshakeTimeout = 30 * time.Second
)

// forwardStatusError 携带应回给客户端的 HTTP 状态码（跨 hub 转发错误映射）。
type forwardStatusError struct {
	status  int
	message string
}

func (e *forwardStatusError) Error() string {
	return fmt.Sprintf("跨 hub 转发失败: HTTP %d: %s", e.status, e.message)
}

// FederationForwarder 跨 hub 中继转发器：把 relay 拨号请求转发到联邦对端 hub。
//
// 并发安全：无共享可变状态（对端 dialer 每 peer 缓存，由 mutex 保护懒加载）；
// PeerForNode 委托 *hub.FederationClient（自身加锁）。无后台 goroutine，
// Close 仅释放引用。
type FederationForwarder struct {
	fc      *hub.FederationClient
	hubID   string // 本 hub 身份（防环路径记录用；为空时不追加路径条目）
	maxHops int
	logger  *slog.Logger
	mu      sync.Mutex
	dialers map[string]*relayForwardDialer // peer.ID → 对端直连 dialer（懒加载）
}

// NewFederationForwarder 创建跨 hub 中继转发器。maxHops<=0 回落默认。
// fc 为 nil 时 PeerForNode 恒返回 false（转发不可用，等价未配置联邦）。
func NewFederationForwarder(fc *hub.FederationClient, hubID string, maxHops int, logger *slog.Logger) *FederationForwarder {
	if maxHops <= 0 {
		maxHops = defaultRelayMaxHops
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &FederationForwarder{
		fc:      fc,
		hubID:   hubID,
		maxHops: maxHops,
		logger:  logger,
		dialers: make(map[string]*relayForwardDialer),
	}
}

// PeerForNode 返回上报目标节点的联邦对端（mesh 严格匹配）。
func (f *FederationForwarder) PeerForNode(id hub.NodeID, mesh string) (hub.FederationPeer, bool) {
	if f == nil || f.fc == nil {
		return hub.FederationPeer{}, false
	}
	return f.fc.PeerForNode(id, mesh)
}

// MaxHops 返回转发链最大跳数。
func (f *FederationForwarder) MaxHops() int {
	if f == nil {
		return 0
	}
	return f.maxHops
}

// Forward 把 relay 拨号请求转发到对端 hub，返回对端已升级的双向字节流。
//
// 防环（DoD 2）：
//   - 跳数超限（incomingHop >= maxHops）→ *forwardStatusError{508}
//   - 目标对端已在路径中（回源/环路）→ *forwardStatusError{508}
//
// 转发时附加 hop+1 / path+hubID 头，供下游 hub 做同样的防环检查。上游非 200
// 状态映射为 *forwardStatusError{状态码}；网络/握手错误映射 502。
func (f *FederationForwarder) Forward(ctx context.Context, peer hub.FederationPeer, target, addr string, incomingHop int, path []string) (net.Conn, error) {
	if incomingHop >= f.maxHops {
		return nil, &forwardStatusError{http.StatusLoopDetected, fmt.Sprintf("跨 hub 转发跳数超限（max=%d）", f.maxHops)}
	}
	if slices.Contains(path, peer.ID) {
		return nil, &forwardStatusError{http.StatusLoopDetected, "检测到转发环路：目标 hub 已在路径中，拒绝回源"}
	}
	nextHop := incomingHop + 1
	nextPath := path
	if f.hubID != "" {
		nextPath = append(append([]string{}, path...), f.hubID)
	}
	headers := map[string]string{
		relayForwardHopHeader:  strconv.Itoa(nextHop),
		relayForwardPathHeader: strings.Join(nextPath, ","),
	}
	d := f.dialerFor(peer)
	conn, err := d.Dial(ctx, peer, target, addr, headers)
	if err != nil {
		return nil, f.mapUpstreamError(peer, err)
	}
	return conn, nil
}

// dialerFor 获取（懒加载）到指定对端的直连 dialer。
func (f *FederationForwarder) dialerFor(peer hub.FederationPeer) *relayForwardDialer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.dialers[peer.ID]; ok {
		return d
	}
	d := &relayForwardDialer{logger: f.logger}
	f.dialers[peer.ID] = d
	return d
}

// mapUpstreamError 把对端 /api/relay/stream 的错误映射为 *forwardStatusError。
// 非 200 状态透传（508 防环、404 目标不存在、502/504 上游失败等）；
// 401/403 映射 502——对端拒绝的是本 hub 的 peer 凭据（网关侧配置错误），
// 不应把「上游未授权」误报成「客户端未授权」。网络/握手错误回落 502。
func (f *FederationForwarder) mapUpstreamError(peer hub.FederationPeer, err error) error {
	var fse *forwardStatusError
	if errors.As(err, &fse) {
		status := fse.status
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			status = http.StatusBadGateway
		}
		return &forwardStatusError{status, fmt.Sprintf("上游 hub %s: %s", peer.ID, fse.message)}
	}
	return &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("连接上游 hub %s 失败: %v", peer.ID, err)}
}

// Close 释放对端直连 dialer 引用。
func (f *FederationForwarder) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialers = make(map[string]*relayForwardDialer)
}

// relayForwardDialer 向对端 hub 发起 /api/relay/stream CONNECT 拨号。
// 与 pkg/client.RelayStream 同协议（raw HTTP CONNECT + SproxySig + 握手 deadline +
// ctx watchdog + 缓冲读），但独立实现以保持 pkg/server 不依赖 pkg/client
// （避免 import cycle：pkg/client 的 e2e 测试反向依赖 pkg/server）。
type relayForwardDialer struct {
	logger *slog.Logger
}

// Dial 建立到对端 hub 的 /api/relay/stream 双向字节流。
func (d *relayForwardDialer) Dial(ctx context.Context, peer hub.FederationPeer, target, addr string, headers map[string]string) (net.Conn, error) {
	body, err := json.Marshal(RelayStreamRequest{Target: target, Type: "tcp", Addr: addr})
	if err != nil {
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("序列化转发请求失败: %v", err)}
	}

	u, err := url.Parse(peer.URL)
	if err != nil {
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("解析对端 URL %q 失败: %v", peer.URL, err)}
	}
	scheme, host := u.Scheme, u.Host
	if host == "" {
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("对端 URL %q 无效", peer.URL)}
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var raw net.Conn
	switch scheme {
	case "https", "wss":
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: relayForwardTLSConfig(peer, host)}
		raw, err = tlsDialer.DialContext(ctx, "tcp", host)
	case "http", "ws":
		raw, err = dialer.DialContext(ctx, "tcp", host)
	default:
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("不支持的对端 URL scheme %q", scheme)}
	}
	if err != nil {
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("连接对端 %s 失败: %v", host, err)}
	}

	// 握手阶段有界（对齐 pkg/client I33）：deadline 取 min(ctx deadline, 30s)；
	// ctx-watchdog 在 ctx 取消时立即关闭底层连接。握手完成后清除 deadline，
	// 长连接数据面不受影响。
	handshakeDeadline := time.Now().Add(relayForwardHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(handshakeDeadline) {
		handshakeDeadline = dl
	}
	if err := raw.SetDeadline(handshakeDeadline); err != nil {
		_ = raw.Close()
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("设置握手 deadline 失败: %v", err)}
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
	cleanup := func() {
		select {
		case <-stopWatchdog:
		default:
			close(stopWatchdog)
		}
		<-watchdogDone
	}
	defer cleanup()

	// 写 CONNECT 风格请求：SproxySig 签名（对端凭据）+ 防环头。
	path := "/api/relay/stream"
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	if peer.AccessKeySecret != "" {
		now := time.Now()
		h := sproxysig.Header{Version: sproxysig.Version, AK: peer.AccessKey,
			TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
			Nonce: sproxysig.NewNonce(), BodySHA256: sproxysig.BodyHash(body)}
		fmt.Fprintf(&b, "Authorization: %s\r\n", sproxysig.SignAndFormat(peer.AccessKeySecret, h, http.MethodPost, path, ""))
	}
	for k, v := range headers {
		// 防 CRLF 注入：拒绝含 \r\n 的头名/值（本端构造，防未来调用方传入脏值）。
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			_ = raw.Close()
			return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("非法转发头 %q（含 CR/LF）", k)}
		}
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, werr := io.WriteString(raw, b.String()); werr != nil {
		_ = raw.Close()
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("写请求头失败: %v", werr)}
	}
	if _, werr := raw.Write(body); werr != nil {
		_ = raw.Close()
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("写请求体失败: %v", werr)}
	}

	// 读响应状态行（CONNECT 风格；不用 http.ReadResponse——200 后紧跟数据面字节，
	// ReadResponse 会把数据当 body 消费，破坏流）。
	br := bufio.NewReader(raw)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		_ = raw.Close()
		return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("读响应状态失败: %v", rerr)}
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	status := 0
	if len(parts) >= 2 {
		status, _ = strconv.Atoi(parts[1])
	}
	if status != http.StatusOK {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		_ = raw.Close()
		reason := strings.TrimSpace(string(rest))
		if reason == "" && len(parts) >= 3 {
			reason = strings.TrimSpace(parts[2])
		}
		return nil, &forwardStatusError{status, reason}
	}
	// 读取剩余响应头直到空行（200 后数据面字节可能已预读进 br，buffered conn 保留）。
	for {
		line, hdrErr := br.ReadString('\n')
		if hdrErr != nil {
			_ = raw.Close()
			return nil, &forwardStatusError{http.StatusBadGateway, fmt.Sprintf("读响应头失败: %v", hdrErr)}
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 清除握手 deadline：长连接数据面（SSH 等）不受残留 deadline 影响。
	_ = raw.SetDeadline(time.Time{})

	// 返回原始连接（bufio.Reader 中可能已缓冲后续数据，包装回 raw）。
	return &relayForwardConn{Conn: raw, reader: br}, nil
}

// relayForwardConn 包装 bufio.Reader（握手时可能已预读数据面字节）为 net.Conn。
// 不支持并发读（bufio.Reader 非并发安全），调用方须保证同一时刻只有一个 goroutine
// Read——与 pkg/client bufferedNetConn 的并发约束一致（S46）。
type relayForwardConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *relayForwardConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// CloseWrite 透传到底层连接（如 *net.TCPConn / *tls.Conn），支持 SSH 等半关闭协议。
// 底层不支持 CloseWrite 时返回错误而非 panic（防御而非崩溃）。
func (c *relayForwardConn) CloseWrite() error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// relayForwardTLSConfig 构造到对端 hub 的 TLS 配置（fail-closed，与联邦拉取一致）：
//   - CAFile 非空 → 用该 CA 构建专属证书池严格校验（ServerName 由 URL host 自动校验）；
//   - InsecureSkipVerify → 跳过校验（仅 loopback peer，Config.Validate 已强制）；
//   - 默认 → 系统根证书池严格校验。
//
// CA 文件读取失败时不设置 RootCAs（走系统根）——联邦拉取构造期已校验过 CA
// （fail-fast），此处仅兜底，TLS 握手失败会以明确错误传播。
func relayForwardTLSConfig(peer hub.FederationPeer, serverName string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	switch {
	case peer.CAFile != "":
		if pem, err := os.ReadFile(peer.CAFile); err == nil {
			if pool := x509.NewCertPool(); pool.AppendCertsFromPEM(pem) {
				cfg.RootCAs = pool
			}
		}
	case peer.InsecureSkipVerify:
		// 仅 loopback peer（Config.Validate 已拒绝远程 + insecure）。
		cfg.InsecureSkipVerify = true //nolint:gosec // 用户仅对本 loopback peer 显式配置跳过证书校验（本机自签开发/测试）
	}
	return cfg
}
