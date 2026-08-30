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
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "tcp",
		Dial:   Dial,
		Listen: Listen,
	})
}

// tcpConn 将 net.Conn 包装为 xfer.Conn，使用 4B 长度前缀帧定界。
type tcpConn struct {
	conn net.Conn
	mu   sync.Mutex // 保护 Send 的并发写入
	// closed 用原子标记：Receive 在阻塞读前无锁检查、Close 在任意时刻调用，
	// 普通 bool + 锁会引入「Receive 无锁读 / Close 有锁写」的数据竞争。
	closed atomic.Bool
}

// maxMessageBytes 是单条 TCP 消息的最大字节数（与 WS 传输对齐，1 MiB）。
// mux 帧（8B 头 + 最多 64 KiB 负载）与 relay 注册/拨号帧均远小于此值；
// 上限用于防止恶意超大长度前缀触发巨型分配（hub 裸 TCP 中继的 DoS 面）。
const maxMessageBytes = 1 << 20

// writeTimeout 是 Send 单次写出的硬超时（对端停止读取时 TCP 写缓冲满，Write 会
// 永久阻塞）。与 WS 传输的 writeTimeout（60s）对齐：远大于 mux 心跳周期（30s
// ping / 90s 超时）与 1 MiB 单条消息在慢链路上的传输时间。ctx 有 deadline 时优先
// 用 ctx deadline；无 deadline（如 mux writeLoop）时用此硬超时兜底，防注册 ACK 等
// 关键帧写入永久占住 hub 连接槽与 goroutine。
const writeTimeout = 60 * time.Second

// Send 发送一条消息：4B 大端长度前缀 + 消息体。
func (c *tcpConn) Send(ctx context.Context, msg []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed.Load() {
		return xfer.ErrConnClosed
	}

	// 写 deadline：ctx deadline 优先，无 deadline 用 writeTimeout 硬超时兜底。
	if err := c.conn.SetWriteDeadline(writeDeadlineFrom(ctx)); err != nil {
		return fmt.Errorf("tcp send set deadline: %w", err)
	}
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()

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

// writeDeadlineFrom 返回 Send 的写截止时间：ctx deadline 优先；无 deadline 时
// 用 writeTimeout 硬超时兜底（防 peer 停读导致写永久阻塞）。
func writeDeadlineFrom(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(writeTimeout)
}

// Receive 阻塞接收一条消息：先读 4B 长度前缀，再读消息体。
//
// ctx deadline 映射为 socket 读 deadline：
//   - 有 deadline（如 hub 注册帧 10s 超时）时读在超时后返回 *net.OpError Timeout，
//     让上层按超时处理——否则「对端连接后不发数据」会永久占住连接槽与 goroutine
//     （hub 裸 TCP 中继的 DoS 面）；
//   - 无 deadline（如 mux readLoop 的 cancel-only ctx）时保持阻塞读，由 conn.Close()
//     解除阻塞（mux.Close 同时调用 conn.Close，与既有收尾路径一致）。
//
// 单条消息上限 maxMessageBytes：超过返回错误（防恶意超大长度前缀触发巨型分配）。
func (c *tcpConn) Receive(ctx context.Context) ([]byte, error) {
	if c.closed.Load() {
		return nil, xfer.ErrConnClosed
	}
	// 读长度前缀：受 ctx deadline 约束
	if err := c.conn.SetReadDeadline(readDeadlineFrom(ctx)); err != nil {
		return nil, fmt.Errorf("tcp recv set deadline: %w", err)
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, lenBuf); err != nil {
		return nil, fmt.Errorf("tcp recv length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen > maxMessageBytes {
		// 已读到非法长度前缀：清除 deadline 后返回，连接随后由调用方关闭。
		_ = c.conn.SetReadDeadline(time.Time{})
		return nil, fmt.Errorf("tcp recv: message too large: %d bytes (max %d)", msgLen, maxMessageBytes)
	}
	// 读消息体：前缀可能跨 ctx deadline 边界，重新应用 deadline
	if err := c.conn.SetReadDeadline(readDeadlineFrom(ctx)); err != nil {
		return nil, fmt.Errorf("tcp recv set deadline: %w", err)
	}
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c.conn, msg); err != nil {
		return nil, fmt.Errorf("tcp recv body: %w", err)
	}
	// 清除读 deadline：长连接数据面（mux 心跳/中继泵送）不受残留 deadline 影响。
	_ = c.conn.SetReadDeadline(time.Time{})
	return msg, nil
}

// readDeadlineFrom 返回 ctx deadline 对应的读截止时间；无 deadline 时返回零值
// （net.Conn.SetReadDeadline 零值表示不设限）。
func readDeadlineFrom(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Time{}
}

// Close 关闭 TCP 连接。
func (c *tcpConn) Close() error {
	if c.closed.Swap(true) {
		return nil // 已关闭（幂等）
	}
	return c.conn.Close()
}

// TcpListener 实现 xfer.Listener，基于 net.Listener。
type TcpListener struct {
	ln        net.Listener
	closeCh   chan struct{}
	closeOnce sync.Once
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

// Close 关闭监听器。可安全多次调用。
func (l *TcpListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closeCh)
	})
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
func Listen(_ context.Context, addr string) (xfer.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	return &TcpListener{
		ln:      ln,
		closeCh: make(chan struct{}),
	}, nil
}
