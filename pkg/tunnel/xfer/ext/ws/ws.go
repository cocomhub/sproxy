// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ws 提供基于 WebSocket 的 xfer.Conn 传输层实现。
//
// 使用 coder/websocket 库，将 WebSocket 连接包装为 xfer.Conn 接口。
// 在 init() 中自动注册到 xfer.TransportRegistry。
package ws

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/coder/websocket"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "ws",
		Dial:   Dial,
		Listen: Listen,
	})
}

// wsConn 将 *websocket.Conn 包装为 xfer.Conn。
// 使用 buffered channel + 后台发送 goroutine 提供发送背压。
// Send 采用两级检查：发送前先检查关闭/取消状态，入 channel 后再次验证，
// 解决 select 非确定性导致的 ContextCancellation/CloseWhileBlocking 竞态。
type wsConn struct {
	conn    *websocket.Conn
	sendCh  chan []byte
	flushCh chan chan error
	closeCh chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

func newWSConn(conn *websocket.Conn) *wsConn {
	c := &wsConn{
		conn:    conn,
		sendCh:  make(chan []byte, 256),
		flushCh: make(chan chan error, 8),
		closeCh: make(chan struct{}),
	}
	// 单条消息上限只在构造时设置一次（coder 内部对上限 +1，见 maxMessageBytes 注释）。
	c.conn.SetReadLimit(maxMessageBytes)
	c.wg.Add(1)
	go c.sendLoop()
	return c
}

func (c *wsConn) sendLoop() {
	defer c.wg.Done()
	// writeMsg 用带超时的 ctx 写出消息：对端停读时 TCP 写缓冲满，coder/websocket
	// 会在超时后关闭整条连接，避免 Write 永久阻塞（见 writeTimeout 注释）。
	writeMsg := func(msg []byte) error {
		wctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		return c.conn.Write(wctx, websocket.MessageBinary, msg)
	}
	for {
		select {
		case msg := <-c.sendCh:
			if err := writeMsg(msg); err != nil {
				// 写失败：回给所有等待中的 flush，广播关闭态，然后退出。
				c.failLoop(err)
				return
			}
		case fl := <-c.flushCh:
			// Flush 请求：先清空已排队的消息再应答（通道 FIFO 保证先于 Flush
			// 调用的 Send 都已入 sendCh），保证对端确实已收到这些帧。
			drainAndAck := func() bool {
				for {
					select {
					case msg := <-c.sendCh:
						if err := writeMsg(msg); err != nil {
							fl <- err
							c.failLoop(err)
							return false
						}
					default:
						fl <- nil
						return true
					}
				}
			}
			if !drainAndAck() {
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

// failLoop 是 sendLoop 写失败后的统一退出路径：先原子标记连接已关闭（P1-14：
// 让 Send 入队后复检 closed 能立即拒绝新入队，不再有"failLoop 到 close(closeCh)
// 之间假成功"的窗口），再把错误回给所有排队中的 Flush，最后广播 closeCh（仅
// 首个完成者 close，幂等）。这样后续 Send/Flush 立即返回错误，不再假成功或悬挂。
func (c *wsConn) failLoop(err error) {
	first := c.markClosed()
	c.failPendingFlushes(err)
	if first {
		close(c.closeCh)
	}
}

// failPendingFlushes 把错误回给所有排队中的 Flush 调用。
// 退出 sendLoop 的动作由调用方 failLoop 完成。
func (c *wsConn) failPendingFlushes(err error) {
	for {
		select {
		case fl := <-c.flushCh:
			fl <- err
		default:
			return
		}
	}
}

// markClosed 原子地标记连接已关闭。返回 true 表示本次调用是首个完成关闭动作的
// （调用方应 close(c.closeCh)）；返回 false 表示已被其他路径关闭，不应重复 close。
func (c *wsConn) markClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	return true
}

// Send 发送一条二进制消息。关闭后返回 ErrConnClosed。
// 两级检查：第一步非阻塞检查 closeCh/ctx.Done() 过滤已关闭/已取消场景；
// 第二步阻塞 select 仅在 sendCh 满时等待 closeCh 或 ctx.Done()。
// 注意：Send 只保证入队，不保证已写出到 socket。需要确保对端收到后再继续
// （或关闭）的关键帧（如注册 REG_ERR），必须随后调用 Flush。
func (c *wsConn) Send(ctx context.Context, msg []byte) error {
	// 第一步：非阻塞前置检查——如果已关闭或 context 已取消，立即返回。
	// 此步骤消除 select 非确定性：ctx 已取消时一定返回错误而非入 channel。
	select {
	case <-c.closeCh:
		return xfer.ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	cp := make([]byte, len(msg))
	copy(cp, msg)

	// 第二步：阻塞发送到 channel，同时监听 closeCh 和 ctx.Done()。
	select {
	case c.sendCh <- cp:
		// P1-14：入队后复检 closed——sendLoop 写失败后 failLoop 会先置 closed 再
		// close(closeCh)，若入队与置位并发（前置 closeCh 检查尚未命中），消息将留在
		// sendCh 永不写出。复检保证连接已失效时 Send 返回错误而非假成功（"Send 返回
		// nil = 已接受"契约不被静默破坏）。注：入队后、写失败前就返回 nil 的消息属
		// 在途丢失（连接失败固有语义），不在本复检范围内。
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return xfer.ErrConnClosed
		}
		return nil
	case <-c.closeCh:
		return xfer.ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush 等待 sendLoop 把队列中所有已 Send 的消息真正写出到 WebSocket，
// 并返回写结果。用于确保对端收到后再继续（或关闭）的关键帧。
// 若底层写失败，返回该错误；连接已关闭或 ctx 取消则返回对应错误。
//
// 注意："已全部写出"的保证仅适用于单发送者（Flush 前无其他并发 Send）。
// 并发发送下，Flush 判定队列空与另一 goroutine 入队之间存在窗口，返回时
// 该消息可能尚未写出——并发发送者需自行同步，Flush 不提供跨发送者保证。
func (c *wsConn) Flush(ctx context.Context) error {
	fl := make(chan error, 1)
	select {
	case c.flushCh <- fl:
	case <-c.closeCh:
		return xfer.ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-fl:
		return err
	case <-c.closeCh:
		return xfer.ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Receive 阻塞接收一条二进制消息。
// 单条消息上限 maxMessageBytes，防恶意超大消息耗尽内存。
func (c *wsConn) Receive(ctx context.Context) ([]byte, error) {
	_, msg, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// maxMessageBytes 是单条 WebSocket 消息的最大字节数（1 MiB），作为本传输的
// 单条消息上限契约：恰好等于上限的消息可正常接收（coder/websocket 的
// SetReadLimit 内部对上限 +1，用于容纳 fin 帧），超过上限对端返回
// ErrMessageTooBig 并关闭连接。调用方发送前应确保单条消息不超过此值。
// mux 帧（8B 头 + 最多 64 KiB 负载）在隧道/流中继下均小于此值；
// 该上限仅用于防止恶意超大单帧耗尽内存。
const maxMessageBytes = 1 << 20

// writeTimeout 是 sendLoop 单次写出的超时。对端停止读取时 TCP 写缓冲满，
// 若不加超时，Write 会永久阻塞导致整条连接假死（心跳帧也无法写出）。
// 60s 远大于 mux 心跳周期（30s ping / 90s 超时），且覆盖 1MiB 单条消息在
// 慢链路（如移动网络）上的传输时间，不会误伤正常长连接。
const writeTimeout = 60 * time.Second

// Close 关闭 WebSocket 连接。
// 先 close(closeCh) 广播关闭信号释放阻塞在 Send 上的 goroutine，
// 再 CloseNow() 关闭底层 socket 中断 sendLoop 中阻塞的 Write，
// 最后等待 sendLoop 退出。
func (c *wsConn) Close() error {
	// markClosed 原子标记关闭；仅首个完成者 close(closeCh)，避免与 sendLoop
	// 写失败路径（failLoop）并发 close 导致 panic。CloseNow 幂等（coder 内部
	// casClosing），重复调用安全。
	if c.markClosed() {
		close(c.closeCh)
	}

	// CloseNow() 关闭底层 socket，中断 sendLoop 中阻塞的 Write。
	err := c.conn.CloseNow()

	// 等待 sendLoop 退出。
	c.wg.Wait()

	return err
}

// DialOptions 是 DialWithOptions 的连接选项。
type DialOptions struct {
	// HTTPClient 用于建立连接。nil 时使用 http.DefaultClient。
	// 需要跳过证书校验（如连接 auto-TLS 自签名 Hub）时，传入配置了
	// TLSClientConfig{InsecureSkipVerify: true} 的自定义 http.Client。
	HTTPClient *http.Client
}

// Dial 创建一个到 WebSocket 服务器的新连接。
// addr 可以是完整的 ws:// 或 wss:// URL，也可以是 host:port 格式。
// host:port 格式会转换为 ws://host:port/ws。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	return DialWithOptions(ctx, addr, DialOptions{})
}

// DialWithOptions 与 Dial 相同，但允许注入 HTTPClient 等连接选项
// （如自定义 TLS 配置，供 sclient --insecure 等场景使用）。
func DialWithOptions(ctx context.Context, addr string, opts DialOptions) (xfer.Conn, error) {
	url := addr
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		url = "ws://" + addr + "/ws"
	}
	var do *websocket.DialOptions
	if opts.HTTPClient != nil {
		do = &websocket.DialOptions{HTTPClient: opts.HTTPClient}
	}
	conn, _, err := websocket.Dial(ctx, url, do)
	if err != nil {
		return nil, err
	}
	return newWSConn(conn), nil
}

// wsListener is the shared implementation for both standalone Listen and
// mounted HandlerNode: it exposes Accept/Close/Addr against a connCh feed.
type wsListener struct {
	srv     *http.Server
	netLn   net.Listener
	connCh  chan xfer.Conn
	closeCh chan struct{}
	closeMu sync.Once
}

// Accept 阻塞等待一个新的 WebSocket 连接。
func (l *wsListener) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case c := <-l.connCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// Close 关闭监听器及其 HTTP 服务器。
func (l *wsListener) Close() error {
	l.closeMu.Do(func() {
		close(l.closeCh)
	})
	return l.srv.Close()
}

// Addr 返回监听器的网络地址。
func (l *wsListener) Addr() net.Addr {
	if l.netLn != nil {
		return l.netLn.Addr()
	}
	return nil
}

// HandlerNode 是可直接挂载到既有 HTTP mux 的 WebSocket 传输节点。
// 与 Listen 不同，它不创建独立 HTTP server，而是把 /{path} 升级端点
// 注册到调用方提供的 mux 上——这样节点连接与文件服务共用同一
// HTTP server（同一端口、TLS 与 Bearer 鉴权），避免孤儿端口。
type HandlerNode struct {
	connCh  chan xfer.Conn
	closeCh chan struct{}
	closeMu sync.Once
}

// NewHandlerNode 创建一个可挂载的 WebSocket 传输节点。
// 调用方随后调用 AddToMux 注册升级端点，并循环 Accept 处理连接。
func NewHandlerNode() *HandlerNode {
	return &HandlerNode{
		connCh:  make(chan xfer.Conn, 16),
		closeCh: make(chan struct{}),
	}
}

// AddToMux 将升级端点注册到指定 http.ServeMux 的 path 上。
func (n *HandlerNode) AddToMux(mux *http.ServeMux, path string) {
	if path == "" {
		path = "/ws"
	}
	// 防御性校验：ServeMux 的 pattern 必须以 / 开头（否则会被当作 host pattern
	// 或直接 panic）。非 / 开头自动补全，与 path=="" 默认 /ws 的容错风格一致。
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 先构造 wsConn（会启动 sendLoop goroutine）；若节点已关闭而走 closeCh
		// 分支，必须调用 wc.Close() 关闭内部 closeCh 释放 sendLoop，避免泄漏。
		wc := newWSConn(conn)
		select {
		case n.connCh <- wc:
		case <-n.closeCh:
			_ = wc.Close()
		}
	})
}

// Accept 阻塞等待一个新的 WebSocket 连接。
func (n *HandlerNode) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case c := <-n.connCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// Close 关闭节点，不再接受新连接。
// 使用 sync.Once 保证并发 Close 只 close(closeCh) 一次，与 wsListener.Close 对齐。
func (n *HandlerNode) Close() error {
	n.closeMu.Do(func() { close(n.closeCh) })
	return nil
}

// Listen 在指定地址启动 WebSocket 监听。
// addr 是 HTTP 监听地址（如 ":8080"）。升级端点固定在 /ws。
func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	netLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &wsListener{
		netLn:   netLn,
		connCh:  make(chan xfer.Conn, 16),
		closeCh: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// 先构造 wsConn（会启动 sendLoop goroutine）；若监听器已关闭而走 closeCh
		// 分支，必须调用 wc.Close() 关闭内部 closeCh 释放 sendLoop，避免泄漏。
		wc := newWSConn(conn)
		select {
		case l.connCh <- wc:
		case <-l.closeCh:
			_ = wc.Close()
		}
	})
	l.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		_ = l.srv.Serve(netLn)
	}()
	return l, nil
}
