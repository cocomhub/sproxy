// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

// mockStream 实现 quic.Stream 接口，用于 quicConn 单元测试。
type mockStream struct {
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer
	closed   bool
	readErr  error
	writeErr error
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

func (m *mockStream) Context() context.Context { return context.Background() }

func (m *mockStream) StreamID() quic.StreamID            { return 0 }
func (m *mockStream) CancelRead(_ quic.StreamErrorCode)  {}
func (m *mockStream) CancelWrite(_ quic.StreamErrorCode) {}
func (m *mockStream) SetDeadline(_ time.Time) error      { return nil }
func (m *mockStream) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockStream) SetWriteDeadline(_ time.Time) error { return nil }

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
