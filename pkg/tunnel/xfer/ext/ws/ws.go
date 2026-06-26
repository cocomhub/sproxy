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
	closeCh chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

func newWSConn(conn *websocket.Conn) *wsConn {
	c := &wsConn{
		conn:    conn,
		sendCh:  make(chan []byte, 256),
		closeCh: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.sendLoop()
	return c
}

func (c *wsConn) sendLoop() {
	defer c.wg.Done()
	for {
		select {
		case msg := <-c.sendCh:
			if err := c.conn.Write(context.Background(), websocket.MessageBinary, msg); err != nil {
				return
			}
		case <-c.closeCh:
			return
		}
	}
}

// Send 发送一条二进制消息。关闭后返回 ErrConnClosed。
// 两级检查：第一步非阻塞检查 closeCh/ctx.Done() 过滤已关闭/已取消场景；
// 第二步阻塞 select 仅在 sendCh 满时等待 closeCh 或 ctx.Done()。
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
		return nil
	case <-c.closeCh:
		return xfer.ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Receive 阻塞接收一条二进制消息。
func (c *wsConn) Receive(ctx context.Context) ([]byte, error) {
	c.conn.SetReadLimit(-1)
	_, msg, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// Close 关闭 WebSocket 连接。
// 先 close(closeCh) 广播关闭信号释放阻塞在 Send 上的 goroutine，
// 再 CloseNow() 关闭底层 socket 中断 sendLoop 中阻塞的 Write，
// 最后等待 sendLoop 退出。
func (c *wsConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// 关闭 closeCh 释放阻塞在 Send 上的 goroutine。
	select {
	case <-c.closeCh:
	default:
		close(c.closeCh)
	}

	// CloseNow() 关闭底层 socket，中断 sendLoop 中阻塞的 Write。
	err := c.conn.CloseNow()

	// 等待 sendLoop 退出。
	c.wg.Wait()

	return err
}

// Dial 创建一个到 WebSocket 服务器的新连接。
// addr 可以是完整的 ws:// 或 wss:// URL，也可以是 host:port 格式。
// host:port 格式会转换为 ws://host:port/ws。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	url := addr
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		url = "ws://" + addr + "/ws"
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	return newWSConn(conn), nil
}

// wsListener 实现 xfer.Listener，基于 HTTP Server 接收 WebSocket 连接。
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
		select {
		case l.connCh <- newWSConn(conn):
		case <-l.closeCh:
			conn.CloseNow()
		}
	})
	l.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		l.srv.Serve(netLn)
	}()
	return l, nil
}
