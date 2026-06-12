// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tcp 提供基于 TCP 的 xfer.Conn 传输层实现。
//
// 使用标准 net 库，采用 4 字节大端长度前缀帧定界，
// 将 TCP 字节流包装为 xfer.Conn 消息接口。
// 在 init() 中自动注册到 xfer 全局注册表，名字为 "tcp"。
package tcp

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "tcp",
		Instance: &xfer.Transport{
			Name:   "tcp",
			Dial:   Dial,
			Listen: Listen,
		},
		Priority: 0,
	})
}

// tcpConn 将 net.Conn 包装为 xfer.Conn，使用 4B 长度前缀帧定界。
type tcpConn struct {
	conn   net.Conn
	mu     sync.Mutex // 保护 Send 的并发写入
	closed bool
}

// Send 发送一条消息：4B 大端长度前缀 + 消息体。
func (c *tcpConn) Send(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return xfer.ErrConnClosed
	}

	// 4B 长度前缀 + payload
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)

	_, err := c.conn.Write(frame)
	if err != nil {
		return fmt.Errorf("tcp send: %w", err)
	}
	return nil
}

// Receive 阻塞接收一条消息：先读 4B 长度前缀，再读消息体。
func (c *tcpConn) Receive(ctx context.Context) ([]byte, error) {
	if c.closed {
		return nil, xfer.ErrConnClosed
	}

	// 读 4B 长度前缀
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, lenBuf); err != nil {
		return nil, fmt.Errorf("tcp recv length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)

	// 读消息体
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c.conn, msg); err != nil {
		return nil, fmt.Errorf("tcp recv body: %w", err)
	}
	return msg, nil
}

// Close 关闭 TCP 连接。
func (c *tcpConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// TcpListener 实现 xfer.Listener，基于 net.Listener。
type TcpListener struct {
	ln      net.Listener
	closeCh chan struct{}
}

// Addr 返回监听器的网络地址。
func (l *TcpListener) Addr() net.Addr {
	return l.ln.Addr()
}

// Accept 阻塞等待一个新的 TCP 连接。
func (l *TcpListener) Accept(ctx context.Context) (xfer.Conn, error) {
	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)

	go func() {
		c, err := l.ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		connCh <- c
	}()

	select {
	case c := <-connCh:
		return &tcpConn{conn: c}, nil
	case err := <-errCh:
		return nil, fmt.Errorf("tcp accept: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

// Close 关闭监听器。
func (l *TcpListener) Close() error {
	close(l.closeCh)
	return l.ln.Close()
}

// Dial 创建到 TCP 服务器的连接。
// addr 格式：host:port（如 "localhost:9000"）。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	return &tcpConn{conn: conn}, nil
}

// Listen 在指定地址启动 TCP 监听。
// addr 格式：:port（如 ":9000"）。
func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	return &TcpListener{
		ln:      ln,
		closeCh: make(chan struct{}),
	}, nil
}
