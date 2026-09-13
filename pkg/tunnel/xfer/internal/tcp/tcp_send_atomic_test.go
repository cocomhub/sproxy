// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// partialWriteConn 是「写 k 字节后失败」的 net.Conn 测试替身：
// 模拟 TCP 短写（部分字节已上线、随后返回错误）。
type partialWriteConn struct {
	mu       sync.Mutex
	wrote    []byte
	closeErr error
	closed   bool
	limit    int // 允许成功写出的字节数；0 = 首次写即失败
}

func (c *partialWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	n := min(c.limit, len(p))
	c.wrote = append(c.wrote, p[:n]...)
	if n < len(p) {
		return n, errors.New("simulated partial write failure")
	}
	return n, nil
}

func (c *partialWriteConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *partialWriteConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.closeErr
}
func (c *partialWriteConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *partialWriteConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *partialWriteConn) SetDeadline(time.Time) error      { return nil }
func (c *partialWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (c *partialWriteConn) SetWriteDeadline(time.Time) error { return nil }

func (c *partialWriteConn) wroteBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.wrote...)
}

func (c *partialWriteConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// TestTcpSend_PartialWrite_ClosesConnAndNeverAppends 钉住 `xfer.Conn` 的「消息边界由实现
// 保证」契约在**短写**下的表现（issue #215）：
//
//	底层 Write 短写并报错时，Send 必须 (a) 返回错误、(b) **关闭连接**。
//
// 为什么必须关连接：半个帧一旦留在线上，任何后续 Send 都会把新帧追加到半截帧之后，对端的
// 长度前缀定界**永久错位**（字节流污染）——上层隧道流是分块加密的，表现为 GCM 认证失败，
// 且重传无法纠正。修前实现是「单次 Write + 失败不关连接」，正是该缺陷。
func TestTcpSend_PartialWrite_ClosesConnAndNeverAppends(t *testing.T) {
	fake := &partialWriteConn{limit: 6} // 长度前缀 4B + 2B payload 后失败
	c := FromNetConn(fake)

	if err := c.Send(context.Background(), []byte("payload")); err == nil {
		t.Fatal("短写应返回错误")
	}
	if !fake.isClosed() {
		t.Fatal("短写失败后必须关闭连接（否则半截帧后会被追加新帧，对端定界永久错位）")
	}
	// 第二次 Send 必须失败（连接已关）——证明不会再往同一字节流追加任何东西。
	if err := c.Send(context.Background(), []byte("second")); err == nil {
		t.Fatal("连接已关闭后 Send 必须失败")
	}
	// 写入字节数恒为 limit（第一次短写的部分；第二次未写入任何字节）。
	if got := len(fake.wroteBytes()); got != 6 {
		t.Fatalf("已写出字节数 = %d, want 6（第二次 Send 不得追加任何字节）", got)
	}
}

// TestTcpSend_WriteErrorBeforeAnyByte_ClosesConn 钉住「一字节未写就失败」也关连接：
// 写错误在流式传输上通常不可恢复（broken pipe 等），继续复用连接只会把错误推迟到更难
// 诊断的位置。
func TestTcpSend_WriteErrorBeforeAnyByte_ClosesConn(t *testing.T) {
	fake := &partialWriteConn{limit: 0}
	c := FromNetConn(fake)

	if err := c.Send(context.Background(), []byte("payload")); err == nil {
		t.Fatal("写失败应返回错误")
	}
	if !fake.isClosed() {
		t.Fatal("写失败后必须关闭连接")
	}
	if got := len(fake.wroteBytes()); got != 0 {
		t.Fatalf("不应写出任何字节, got %d", got)
	}
}

// TestTcpSend_ShortWriteThenComplete 钉住「短写但最终成功」的正常路径：WriteFull 循环写足，
// 不关连接、返回 nil，且对端收到的字节与帧完全一致（长度前缀 + payload）。
func TestTcpSend_ShortWriteThenComplete(t *testing.T) {
	fake := &shortWriteConn{limit: 3}
	c := FromNetConn(fake)

	if err := c.Send(context.Background(), []byte("hello")); err != nil {
		t.Fatalf("短写后写足应成功, got %v", err)
	}
	if fake.isClosed() {
		t.Fatal("成功路径不得关闭连接")
	}
	want := append([]byte{0, 0, 0, 5}, []byte("hello")...)
	if got := fake.wroteBytes(); string(got) != string(want) {
		t.Fatalf("线上字节 = %v, want %v（4B 长度前缀 + payload）", got, want)
	}
}

// shortWriteConn 是「每次只写 limit 字节、但从不失败」的 net.Conn（模拟窗口受限短写）。
type shortWriteConn struct {
	mu     sync.Mutex
	wrote  []byte
	closed bool
	limit  int
}

func (c *shortWriteConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	n := min(c.limit, len(p))
	c.wrote = append(c.wrote, p[:n]...)
	return n, nil
}

func (c *shortWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *shortWriteConn) Close() error                     { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *shortWriteConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *shortWriteConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *shortWriteConn) SetDeadline(time.Time) error      { return nil }
func (c *shortWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (c *shortWriteConn) SetWriteDeadline(time.Time) error { return nil }

func (c *shortWriteConn) wroteBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.wrote...)
}

func (c *shortWriteConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
