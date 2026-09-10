// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// mockStream 实现 streamInterface（io.Reader + io.Writer + io.Closer + 读 deadline），
// 用于 quicConn 单元测试。
type mockStream struct {
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer
	closed   bool
	readErr  error
	writeErr error

	deadlineMu sync.Mutex
	deadline   time.Time
}

func (m *mockStream) Read(p []byte) (int, error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	return m.readBuf.Read(p)
}

func (m *mockStream) Write(p []byte) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return m.writeBuf.Write(p)
}

func (m *mockStream) Close() error {
	m.closed = true
	return nil
}

// SetReadDeadline 记录读 deadline（quicConn 借它把 ctx 兑现为读超时）。
func (m *mockStream) SetReadDeadline(t time.Time) error {
	m.deadlineMu.Lock()
	defer m.deadlineMu.Unlock()
	m.deadline = t
	return nil
}

// readDeadline 返回当前读 deadline（-race 下测试读取用）。
func (m *mockStream) readDeadline() time.Time {
	m.deadlineMu.Lock()
	defer m.deadlineMu.Unlock()
	return m.deadline
}

func writeFrame(buf *bytes.Buffer, msg []byte) {
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	buf.Write(frame)
}

func TestQuicConnSend(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	msg := []byte("hello")
	if err := c.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// 读取写入的帧
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(&s.writeBuf, lenBuf); err != nil {
		t.Fatal(err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	body := make([]byte, msgLen)
	if _, err := io.ReadFull(&s.writeBuf, body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, msg) {
		t.Fatalf("expected %q, got %q", msg, body)
	}
}

func TestQuicConnReceive(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	msg := []byte("world")
	writeFrame(&s.readBuf, msg)

	got, err := c.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("expected %q, got %q", msg, got)
	}
}

func TestQuicConnRoundTrip(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	msg := []byte("round-trip")
	if err := c.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// 把写入的数据搬到读缓冲区，模拟对端收到了相同的数据
	s.readBuf = s.writeBuf

	got, err := c.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("expected %q, got %q", msg, got)
	}
}

func TestQuicConnSendAfterClose(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	err := c.Send(context.Background(), []byte("after close"))
	if err == nil {
		t.Fatal("expected error when sending after close")
	}
	if !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("expected ErrConnClosed, got %v", err)
	}
}

func TestQuicConnReceiveAfterClose(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := c.Receive(context.Background())
	if err == nil {
		t.Fatal("expected error when receiving after close")
	}
	if !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("expected ErrConnClosed, got %v", err)
	}
}

func TestQuicConnIdempotentClose(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if !s.closed {
		t.Fatal("stream should be closed after first Close")
	}

	// 第二次 Close 应返回 nil 且不 panic
	if err := c.Close(); err != nil {
		t.Fatalf("second close should be idempotent, got: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("third close should be idempotent, got: %v", err)
	}
}

func TestQuicConnEmptyMessage(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	if err := c.Send(context.Background(), []byte{}); err != nil {
		t.Fatal(err)
	}

	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(&s.writeBuf, lenBuf); err != nil {
		t.Fatal(err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen != 0 {
		t.Fatalf("expected empty message length 0, got %d", msgLen)
	}
}

func TestQuicConnLargePayload(t *testing.T) {
	s := &mockStream{}
	c := &quicConn{stream: s}

	payload := make([]byte, 64*1024) // 64 KB
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	if err := c.Send(context.Background(), payload); err != nil {
		t.Fatal(err)
	}

	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(&s.writeBuf, lenBuf); err != nil {
		t.Fatal(err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	if msgLen != uint32(len(payload)) {
		t.Fatalf("expected length %d, got %d", len(payload), msgLen)
	}
	body := make([]byte, msgLen)
	if _, err := io.ReadFull(&s.writeBuf, body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatal("large payload mismatch")
	}
}

func TestQuicConnSendError(t *testing.T) {
	s := &mockStream{writeErr: io.ErrShortWrite}
	c := &quicConn{stream: s}

	err := c.Send(context.Background(), []byte("test"))
	if err == nil {
		t.Fatal("expected error when stream write fails")
	}
}

func TestQuicConnReceiveReadError(t *testing.T) {
	s := &mockStream{readErr: io.ErrUnexpectedEOF}
	c := &quicConn{stream: s}

	_, err := c.Receive(context.Background())
	if err == nil {
		t.Fatal("expected error when stream read fails")
	}
}

// blockingStream 模拟 quic.Stream 的阻塞读：Read 一直阻塞直到读 deadline 到期
// （SetReadDeadline 生效），用于验证 quicConn.Receive 对 ctx 的兑现。
type blockingStream struct {
	mu       sync.Mutex
	deadline time.Time
	closed   bool
}

func (s *blockingStream) Read(_ []byte) (int, error) {
	for {
		s.mu.Lock()
		dl, closed := s.deadline, s.closed
		s.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		if !dl.IsZero() && !time.Now().Before(dl) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *blockingStream) Write(p []byte) (int, error) { return len(p), nil }

func (s *blockingStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *blockingStream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = t
	return nil
}

// TestQuicConnReceiveWithDeadline 验证 Receive 用 ctx deadline 兑现阻塞读：
// 必须在毫秒级返回错误，而不是阻塞到对端发送数据或连接空闲超时。
func TestQuicConnReceiveWithDeadline(t *testing.T) {
	c := &quicConn{stream: &blockingStream{}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Receive(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for expired ctx deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Receive 未兑现 ctx deadline，耗时 %v", elapsed)
	}
}

// TestQuicConnReceiveWithCancel 验证 Receive 用 ctx 取消解除阻塞读（无 deadline 的 ctx）。
func TestQuicConnReceiveWithCancel(t *testing.T) {
	c := &quicConn{stream: &blockingStream{}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Receive(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Receive 未兑现 ctx 取消，耗时 %v", elapsed)
	}
}

// TestQuicConnReceiveClearsDeadline 验证读结束后读 deadline 被复位，
// 不留残余 deadline 影响后续长连接数据面。
func TestQuicConnReceiveClearsDeadline(t *testing.T) {
	s := &mockStream{}
	writeFrame(&s.readBuf, []byte("ok"))
	c := &quicConn{stream: s}

	got, err := c.Receive(context.Background())
	if err != nil {
		t.Fatalf("receive failed: %v", err)
	}
	if string(got) != "ok" {
		t.Fatalf("expected %q, got %q", "ok", got)
	}
	if dl := s.readDeadline(); !dl.IsZero() {
		t.Fatalf("read deadline should be cleared after Receive, got %v", dl)
	}
}

// TestQuicConnReceiveNoGoroutineLeak 验证兑现 ctx 取消的 watcher goroutine
// 在读结束后退出（不随调用次数累积）。
func TestQuicConnReceiveNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	const rounds = 50

	for range rounds {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(2 * time.Millisecond)
			cancel()
		}()
		c := &quicConn{stream: &blockingStream{}}
		if _, err := c.Receive(ctx); err == nil {
			t.Fatal("expected error from cancelled ctx")
		}
	}

	// watcher 与 cancel goroutine 的退出是异步的，给一点收敛时间再判定。
	for range 50 {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: base=%d now=%d", base, runtime.NumGoroutine())
}
