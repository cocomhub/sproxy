// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

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

// handshakeTimeout 是单个连接的 TLS 握手超时（对齐 quic 传输的
// HandshakeIdleTimeout=30s 与阶段 5 设计文档的"握手 30s 超时"边界）。
// 未认证/慢速对端最多占住一次握手，不能无限阻塞 accept 循环（C-1 DoS 收敛）。
const handshakeTimeout = 30 * time.Second

// defaultTLSConfig 是 "tcp+tls" 变体的默认 *tls.Config（客户端 DialTLS / 服务端
// ListenTLS 未显式传参时使用）。atomic.Pointer 保证运行期热设置安全
// （SIGHUP 软配置重载先例；M-1 并发读安全）。nil 时变体 Dial/Listen 返回明确错误。
var defaultTLSConfig atomic.Pointer[tls.Config]

// SetDefaultTLSConfig 设置 "tcp+tls" 变体的默认 *tls.Config（客户端 DialTLS /
// 服务端 ListenTLS 未显式传参时使用）。传 nil 使变体 Dial/Listen 返回明确错误。
func SetDefaultTLSConfig(cfg *tls.Config) {
	defaultTLSConfig.Store(cfg)
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
	if defaultTLSConfig.Load() == nil {
		return nil, fmt.Errorf("tcp+tls: 未设置 TLS 配置（先调用 tcp.SetDefaultTLSConfig 或使用 tcp.DialTLS）")
	}
	return DialTLS(ctx, addr, defaultTLSConfig.Load())
}

// listenTLSRegistered 是 "tcp+tls" 变体的 Registry Listen 回调。
func listenTLSRegistered(ctx context.Context, addr string) (xfer.Listener, error) {
	if defaultTLSConfig.Load() == nil {
		return nil, fmt.Errorf("tcp+tls: 未设置 TLS 配置（先调用 tcp.SetDefaultTLSConfig 或使用 tcp.ListenTLS）")
	}
	return ListenTLS(ctx, addr, defaultTLSConfig.Load())
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
	// TLS 1.2 下限（与 ListenTLS 对称；Go 1.18+ 客户端默认已是 1.2，显式兜底防配置覆盖）。
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
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
		connCh:      make(chan xfer.Conn, 8),
		errCh:       make(chan error, 1),
	}, nil
}

// TlsListener 是 TLS 包装的 TcpListener。
//
// 设计：握手在后台 goroutine 并发进行——accept 循环接受裸连接后，每个连接独立
// goroutine 执行 tls.Server 握手（带 handshakeTimeout），成功入队、失败关闭跳过。
// 单条停顿/恶意连接只占住自己的握手 goroutine，不阻塞 accept 循环与后续合法连接
// （C-1 未认证单连接 DoS 收敛；I-1 无递归栈增长）。
type TlsListener struct {
	*TcpListener
	cfg *tls.Config

	connCh chan xfer.Conn // 握手成功的连接队列（缓冲防握手突发积压阻塞）
	errCh  chan error     // listener 级致命错误（accept 循环退出时一次性上报）

	loopOnce sync.Once
}

// Accept 阻塞返回一个已完成 TLS 握手的连接。
// 握手失败的连接被内部丢弃；listener 关闭返回 ErrConnClosed；ctx 取消返回 ctx.Err()。
func (l *TlsListener) Accept(ctx context.Context) (xfer.Conn, error) {
	l.loopOnce.Do(func() { go l.acceptLoop() })

	select {
	case c := <-l.connCh:
		return c, nil
	case err := <-l.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// acceptLoop 从底层 TcpListener 接受裸连接，每个连接起独立 goroutine 并发握手。
// 阻塞直到 listener 关闭（ErrConnClosed）或不可恢复错误。
func (l *TlsListener) acceptLoop() {
	for {
		conn, err := l.TcpListener.Accept(context.Background())
		if err != nil {
			if !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, net.ErrClosed) {
				select {
				case l.errCh <- err:
				default:
				}
			}
			return // listener 已关闭或不可恢复错误：退出循环
		}
		go l.handshakeConn(conn)
	}
}

// handshakeConn 对单个连接执行 TLS 握手（带 handshakeTimeout），成功后入队。
// 失败关闭该连接（恶意对端不能让 Listener 停摆）；listener 已关闭时丢弃成功连接。
func (l *TlsListener) handshakeConn(conn xfer.Conn) {
	raw, ok := underlyingConn(conn)
	if !ok || raw == nil {
		_ = conn.Close()
		return
	}
	tlsConn := tls.Server(raw, l.cfg)
	// 握手超时：HandshakeContext 在 ctx 取消/deadline 时中断握手。
	hctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err := tlsConn.HandshakeContext(hctx)
	cancel()
	if err != nil {
		_ = raw.Close()
		return // 握手失败：跳过该连接继续接受
	}
	select {
	case l.connCh <- &tcpConn{conn: tlsConn}:
	case <-l.closeCh:
		_ = tlsConn.Close() // listener 已关闭：丢弃
	}
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
