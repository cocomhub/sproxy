// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// 阶段 5 工作项 1：给 tcp 传输加 TLS 变体 "tcp+tls"。
//
// 变体注册：tcp.go 的 init() 已注册裸 "tcp"；本文件 init() 额外注册 "tcp+tls"。
// Registry 的 Dial/Listen 不接受配置参数，故 "tcp+tls" 变体使用包级 defaultTLSConfig：
//   - 服务端/客户端装配时先调用 SetDefaultTLSConfig(cfg)；
//   - 未设置时变体 Dial/Listen 返回明确错误（fail-closed，禁止"无凭据静默明文"）。
//   - 直接函数 DialTLS/ListenTLS 接受显式 *tls.Config，供装配点注入（激进路径）。
//
// 与既有 wss 先例一致：ws 传输的 DialWithOptions 也是包级配置注入模式。
var defaultTLSConfig *tls.Config

// SetDefaultTLSConfig 设置 "tcp+tls" 变体的默认 *tls.Config（客户端 DialTLS /
// 服务端 ListenTLS 未显式传参时使用）。传 nil 使变体 Dial/Listen 返回明确错误。
func SetDefaultTLSConfig(cfg *tls.Config) {
	defaultTLSConfig = cfg
}

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "tcp+tls",
		Dial:   dialTLSRegistered,
		Listen: listenTLSRegistered,
	})
}

// dialTLSRegistered 是 "tcp+tls" 变体的 Registry Dial 回调（缺省配置下报错）。
func dialTLSRegistered(ctx context.Context, addr string) (xfer.Conn, error) {
	if defaultTLSConfig == nil {
		return nil, fmt.Errorf("tcp+tls: 未设置 TLS 配置（先调用 tcp.SetDefaultTLSConfig 或使用 tcp.DialTLS）")
	}
	return DialTLS(ctx, addr, defaultTLSConfig)
}

// listenTLSRegistered 是 "tcp+tls" 变体的 Registry Listen 回调。
func listenTLSRegistered(ctx context.Context, addr string) (xfer.Listener, error) {
	if defaultTLSConfig == nil {
		return nil, fmt.Errorf("tcp+tls: 未设置 TLS 配置（先调用 tcp.SetDefaultTLSConfig 或使用 tcp.ListenTLS）")
	}
	return ListenTLS(ctx, addr, defaultTLSConfig)
}

// DialTLS 创建到 TLS TCP 服务器的连接（addr 格式 host:port）。
// tlsCfg 必须设置 RootCAs / ServerName 以校验对端证书；InsecureSkipVerify 仅允许
// loopback（由调用方按 fail-closed 限制）。
func DialTLS(ctx context.Context, addr string, tlsCfg *tls.Config) (xfer.Conn, error) {
	if tlsCfg == nil {
		return nil, fmt.Errorf("tcp+tls dial: nil tls.Config")
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp+tls dial: %w", err)
	}
	cfg := tlsCfg.Clone()
	if cfg.ServerName == "" {
		if host, _, hErr := net.SplitHostPort(addr); hErr == nil {
			cfg.ServerName = host
		}
	}
	tlsConn := tls.Client(raw, cfg)
	// 显式握手：立即暴露证书校验失败（而非等首条消息时偶发返回）。
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tcp+tls dial handshake: %w", err)
	}
	return &tcpConn{conn: tlsConn}, nil
}

// ListenTLS 在 addr（:port）上启动 TLS TCP 监听。tlsCfg 须含证书（GetCertificate，
// Certificates 或 CertPool 自签信任）。
func ListenTLS(ctx context.Context, addr string, tlsCfg *tls.Config) (xfer.Listener, error) {
	if tlsCfg == nil {
		return nil, fmt.Errorf("tcp+tls listen: nil tls.Config")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp+tls listen: %w", err)
	}
	cfg := tlsCfg.Clone()
	// TLS 1.2 下限（对齐仓库其他传输层的 TLS 配置基线）。
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return &TlsListener{
		TcpListener: &TcpListener{ln: ln, closeCh: make(chan struct{})},
		cfg:         cfg,
	}, nil
}

// TlsListener 是 TLS 包装的 TcpListener：Accept 时为每个裸连接做 tls.Server 握手。
type TlsListener struct {
	*TcpListener
	cfg *tls.Config
}

// Accept 阻塞接受一个新连接，并在返回前完成 TLS 握手。
// 握手失败（证书拒绝/协议错位）关闭该连接并继续等待下一连接（恶意对端不能
// 让 Listener 停摆）；监听器关闭/ctx 取消时返回终态错误（由 TcpListener.Accept 决定）。
func (l *TlsListener) Accept(ctx context.Context) (xfer.Conn, error) {
	conn, err := l.TcpListener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	raw, ok := underlyingConn(conn)
	if !ok || raw == nil {
		return nil, fmt.Errorf("tcp+tls accept: 无法取到底层连接")
	}
	tlsConn := tls.Server(raw, l.cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return l.Accept(ctx) // 握手失败：跳过该连接继续接受
	}
	// 重新包装：用 tlsConn 替换底层，使上层解密的字节流过 tls 层。
	return &tcpConn{conn: tlsConn}, nil
}

// underlyingConn 从 TcpListener.Accept 返回的 *tcpConn 提取底层 net.Conn。
// 返回 (conn, ok)。非 *tcpConn（理论上不可能，防御性）返回 (nil, false)。
func underlyingConn(c xfer.Conn) (net.Conn, bool) {
	tc, ok := c.(*tcpConn)
	if !ok || tc.conn == nil {
		return nil, false
	}
	return tc.conn, true
}
