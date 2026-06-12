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

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/coder/websocket"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "ws",
		Instance: &xfer.Transport{
			Name:   "ws",
			Dial:   Dial,
			Listen: Listen,
		},
		Priority: 10,
	})
}

// wsConn 将 *websocket.Conn 包装为 xfer.Conn。
// 内部使用有界 channel + 后台发送 goroutine，提供发送背压支持。
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

// Send 发送一条二进制消息。当缓冲区满时阻塞；关闭后返回 ErrConnClosed。
func (c *wsConn) Send(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return xfer.ErrConnClosed
	}
	c.mu.Unlock()

	select {
	case c.sendCh <- msg:
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
func (c *wsConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// 先关闭底层连接使阻塞的 Write 返回，再通知发送循环退出
	err := c.conn.CloseNow()
	select {
	case <-c.closeCh:
	default:
		close(c.closeCh)
	}
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
