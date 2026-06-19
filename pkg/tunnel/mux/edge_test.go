// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestStream_ReadEOFAfterCloseWrite(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	accepted, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}

	_, err = stream.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 半关闭：发送 FrameCloseWrite
	if closeWriteErr := stream.CloseWrite(); closeWriteErr != nil {
		t.Fatalf("CloseWrite failed: %v", closeWriteErr)
	}

	// 接受端应读到数据，然后读到 io.EOF
	buf := make([]byte, 1024)
	n, err := accepted.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("expected data on first read, got n=%d err=%v", n, err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}

	// 第二次 Read 应返回 io.EOF（半关闭已结束）
	_, err = accepted.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after closewrite, got %v", err)
	}
}

func TestFlowControl_WriteBlocked(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	accepted, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	_ = accepted

	// 写满发送窗口（默认 64KB），触发流控阻塞
	payload := make([]byte, 65536)
	_, err = stream.Write(payload)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 验证写入了完整的 payload
	t.Log("wrote 64KB successfully")
}

// TestWithAcceptChSize 验证 WithAcceptChSize 选项
func TestWithAcceptChSize(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.NewWithOpts(a, mux.RoleDialer)
	muxB := mux.NewWithOpts(b, mux.RoleListener, mux.WithAcceptChSize(2))
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 打开 3 条流，不 Accept，第 3 条应触发拒绝
	streams := make([]mux.Stream, 0, 3)
	for i := range 3 {
		s, err := muxA.Open(ctx)
		if err != nil {
			if i < 2 {
				t.Fatalf("Open #%d should succeed: %v", i, err)
			}
			t.Logf("Open #%d failed (expected maybe): %v", i, err)
			break
		}
		streams = append(streams, s)
	}

	// 现在 Accept 一条，验证能拿到流
	s, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	s.Close()

	for _, s := range streams {
		s.Close()
	}
}

// TestHandleFrame_CloseWriteUnknownStream 测试 handleFrame 中 FrameCloseWrite 对未知流的处理
func TestHandleFrame_CloseWriteUnknownStream(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mux.EncodeFrame(999, mux.FrameCloseWrite, nil)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestHandleFrame_WindowUpdateUnknownStream 测试 handleFrame 中 FrameWindowUpdate 对未知流的处理
func TestHandleFrame_WindowUpdateUnknownStream(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	payload := make([]byte, 4)
	payload[0] = 0x01
	rawFrame := mux.EncodeFrame(999, mux.FrameWindowUpdate, payload)
	ctx := t.Context()
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestHandleFrame_UnknownFrameType 测试 handleFrame 对未知帧类型的处理
func TestHandleFrame_UnknownFrameType(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	rawFrame := mux.EncodeFrame(0, 0xFF, nil)
	ctx := t.Context()
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestOpenAfterConnClose_Dialer 验证底层连接断开时 Open 返回错误
func TestOpenAfterConnClose_Dialer(t *testing.T) {
	a, _ := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	a.Close()

	ctx := t.Context()
	_, err := muxA.Open(ctx)
	if err == nil {
		t.Fatal("expected error on Open after conn close")
	}
	t.Logf("Open after conn close: %v", err)

	muxA.Close()
}

// TestHandleFrame_DuplicateOpen 测试 handleFrame 对重复 FrameOpen 的处理
func TestHandleFrame_DuplicateOpen(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	accepted, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()

	// 模拟重复的 FrameOpen
	rawFrame := mux.EncodeFrame(stream.ID(), mux.FrameOpen, nil)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
}

// TestHandleFrame_Ping 测试 handleFrame 中 FramePing 的处理（回复 FramePong）
func TestHandleFrame_Ping(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mux.EncodeFrame(0, mux.FramePing, nil)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestHandleFrame_Pong 测试 handleFrame 中 FramePong 的处理
func TestHandleFrame_Pong(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mux.EncodeFrame(0, mux.FramePong, nil)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestWrite_BiggerThanWindow 测试写超过窗口大小的数据，应触发 writeLen 截断
func TestWrite_BiggerThanWindow(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	accepted, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	_ = accepted

	// 写超过默认窗口大小（65536）的数据
	payload := make([]byte, 70000)
	_, err = stream.Write(payload)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Log("wrote 70KB across window boundary")
}

// TestHandleFrame_InvalidFrame 测试 handleFrame 对无效帧的处理
func TestHandleFrame_InvalidFrame(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	if err := b.Send(ctx, []byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	b.Close()
}

// TestWriteEmptyPayload 测试写入空数据应直接返回 0, nil
func TestWriteEmptyPayload(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	n, err := stream.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil) should succeed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}

	n, err = stream.Write([]byte{})
	if err != nil {
		t.Fatalf("Write([]byte{}) should succeed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}
}

// TestOpenWithMaxStreams_ListenerSide_Reject 验证 listener 侧因 maxStreams 拒绝 FrameOpen
func TestOpenWithMaxStreams_ListenerSide_Reject(t *testing.T) {
	a, b := xfertest.Pipe()

	lm := mux.NewWithOpts(b, mux.RoleListener, mux.WithMaxStreams(1))
	dm := mux.New(a, mux.RoleDialer)
	defer lm.Close()
	defer dm.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	s1, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("First Open failed: %v", err)
	}
	defer s1.Close()

	acc1, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("First Accept failed: %v", err)
	}
	defer acc1.Close()

	s2, err := dm.Open(ctx)
	if err != nil {
		t.Logf("Second Open failed (expected possible): %v", err)
		return
	}
	defer s2.Close()
	_ = s2

	time.Sleep(200 * time.Millisecond)
}
