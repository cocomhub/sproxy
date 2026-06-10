// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xferws 提供基于 WebSocket 的 xfer.Conn 传输层实现。
//
// 使用 coder/websocket 库，将 WebSocket 连接包装为 xfer.Conn 接口。
// 在 init() 中自动注册到 xfer 全局注册表，名字为 "ws"。
package xferws

import (
	"context"
	"net"
	"net/http"

	"github.com/coder/websocket"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "ws",
		Dial:   Dial,
		Listen: Listen,
	})
}

// wsConn 将 *websocket.Conn 包装为 xfer.Conn。
type wsConn struct {
	conn *websocket.Conn
}

// Send 发送一条二进制消息。
func (c *wsConn) Send(ctx context.Context, msg []byte) error {
	return c.conn.Write(ctx, websocket.MessageBinary, msg)
}

// Receive 阻塞接收一条二进制消息。
func (c *wsConn) Receive(ctx context.Context) ([]byte, error) {
	_, msg, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// Close 关闭 WebSocket 连接。
func (c *wsConn) Close() error {
	return c.conn.CloseNow()
}

// Dial 创建一个到 WebSocket 服务器的新连接。
// addr 必须是完整的 ws:// 或 wss:// URL。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{conn: conn}, nil
}

// wsListener 实现 xfer.Listener，基于 HTTP Server 接收 WebSocket 连接。
type wsListener struct {
	srv     *http.Server
	netLn   net.Listener
	connCh  chan xfer.Conn
	closeCh chan struct{}
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
	close(l.closeCh)
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
// addr 是 HTTP 监听地址（如 ":8080"）。
// 升级端点固定在 /ws。
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
		case l.connCh <- &wsConn{conn: conn}:
		case <-l.closeCh:
			conn.CloseNow()
		}
	})
	l.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	go func() {
		l.srv.Serve(netLn)
	}()
	return l, nil
}
