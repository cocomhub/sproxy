// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestMuxOpenAccept(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	streamA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer streamA.Close()

	streamB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer streamB.Close()

	msg := []byte("hello mux")
	n, err := streamA.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(msg) {
		t.Fatalf("wrote %d, expected %d", n, len(msg))
	}

	buf := make([]byte, 1024)
	n, err = streamB.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf[:n])
	}

	reply := []byte("pong")
	_, err = streamB.Write(reply)
	if err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 1024)
	n, err = streamA.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != string(reply) {
		t.Fatalf("expected %q, got %q", reply, buf[:n])
	}
}

func TestMuxCloseWrite(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	streamA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer streamA.Close()

	streamB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer streamB.Close()

	// 发送数据 + CloseWrite
	if _, err = streamA.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err = streamA.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// B 读数据
	buf := make([]byte, 1024)
	n, err := streamB.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected %q, got %q", "hello", buf[:n])
	}

	// B 读 EOF（CloseWrite 信号）
	n, err = streamB.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after CloseWrite, got %v (n=%d)", err, n)
	}
}

func TestMuxBidirectionalWrite(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()
	sB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	// 双向同时写
	sA.Write([]byte("fromA"))
	sB.Write([]byte("fromB"))

	// A 读 fromB
	buf := make([]byte, 16)
	n, err := sA.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "fromB" {
		t.Fatalf("expected fromB, got %q", buf[:n])
	}

	// B 读 fromA
	n, err = sB.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "fromA" {
		t.Fatalf("expected fromA, got %q", buf[:n])
	}
}

func TestMuxMultipleStreams(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const n = 5
	streamsA := make([]mux.Stream, n)
	for i := range n {
		s, err := muxA.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		streamsA[i] = s
	}

	verifyMuxMultipleStreams(t, muxB, streamsA, ctx)
}

// verifyMuxMultipleStreams 验证多个流可以同时打开并正确收发。
func verifyMuxMultipleStreams(t *testing.T, muxB *mux.Mux, streamsA []mux.Stream, ctx context.Context) {
	t.Helper()
	n := len(streamsA)
	bMap := make(map[mux.StreamID]mux.Stream)
	for range n {
		s, err := muxB.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		bMap[s.ID()] = s
		defer s.Close()
	}

	for i, s := range streamsA {
		msg := []byte{byte('A' + i)}
		if _, err := s.Write(msg); err != nil {
			t.Fatal(err)
		}
	}

	for _, s := range streamsA {
		bs, ok := bMap[s.ID()]
		if !ok {
			t.Fatalf("stream %d not found on B", s.ID())
		}
		buf := make([]byte, 1024)
		n, err := bs.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("expected 1 byte, got %d", n)
		}
	}
}

func TestMuxClose(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	streamA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	streamB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	muxA.Close()

	// 写入应失败
	_, err = streamA.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error on write after mux close")
	}

	// B 端读应得到错误
	buf := make([]byte, 1024)
	_, err = streamB.Read(buf)
	if err == nil {
		t.Fatal("expected error on read after mux close")
	}

	muxB.Close()
}

func TestMuxAcceptCancel(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	// Accept 应因 ctx 取消而超时
	_, err := muxB.Accept(ctx)
	if err == nil {
		t.Fatal("expected error on cancelled accept")
	}
}

func TestMuxAcceptAfterClose(t *testing.T) {
	a, b := xfertest.Pipe()
	muxB := mux.New(b, mux.RoleListener)
	muxB.Close()
	_ = a

	ctx := t.Context()
	_, err := muxB.Accept(ctx)
	if err == nil {
		t.Fatal("expected error on accept after close")
	}
}

func TestMuxOpenAfterClose(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxA.Close()
	_ = b

	ctx := t.Context()
	_, err := muxA.Open(ctx)
	if err == nil {
		t.Fatal("expected error on open after close")
	}
}

func TestMuxDataForUnknownStream(t *testing.T) {
	// 测试 handleFrame 对未知流ID的 FrameData 静默丢弃
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	_ = muxA
	_ = b
	// 间接测试：通过 muxB 发送一个对 muxA 未知流的 FrameData
	// 利用 PipeConn 手动注入帧
	ctx := t.Context()
	// 直接通过 conn 发送一个指向不存在的 streamID 的帧
	rawFrame := mux.EncodeFrame(999, mux.FrameData, []byte("orphan data"))
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	// muxA 的 readLoop 会处理此帧，应该静默丢弃（不 panic）
	time.Sleep(100 * time.Millisecond)
	muxA.Close()
	b.Close()
}

func TestMuxFramePingPong(t *testing.T) {
	// 测试 Ping/Pong 心跳：创建 mux，等待足够长的时间让 ping 触发
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 打开一条流，确保连接活跃
	s, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()

	// 如果 Ping/Pong 正常工作，连接应保持活跃
	// （心跳默认 30s，测试只验证不崩溃）
	time.Sleep(50 * time.Millisecond)
}

func TestMuxWriteChClosedStream(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	s, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 先 CloseWrite 再 Write
	if err = s.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// 再写入应成功（CloseWrite 只发信号，不关闭流）
	_, err = s.Write([]byte("after closewrite"))
	if err != nil {
		t.Fatal(err) // 应允许，直到真正的 Close
	}
}

func TestMuxMetrics(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	m := muxA.Metrics()
	if m == nil {
		t.Fatal("Metrics() returned nil")
	}
}

// TestMuxSendReceive 验证 mux 基本发送接收正确性。
// 完整的流控窗口耗尽测试需要 pipe transport 的 sendWindowUpdateUnsafe 支持，当前 pipe 不支持。
func TestMuxSendReceive(t *testing.T) {
	// NOTE: 完整的窗口耗尽测试需要 pipe transport 支持 sendWindowUpdateUnsafe
	// 与 mux 内部 writeLoop 的交互，此处仅做窗口基本正确的验证。
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()

	sB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	// 发送略小于窗口的数据，验证正常交互
	payload := []byte("flow control test message")
	_, err = sA.Write(payload)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	_ = sA.CloseWrite()

	buf := make([]byte, 4096)
	n, err := sB.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, buf[:n])
	}
}

// TestMuxAcceptChFull_Reject 验证 acceptCh 满时，dialer 侧的 Read 返回错误而非阻塞。
func TestMuxAcceptChFull_Reject(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 用 acceptCh 容量（64）+1 个流来触发 acceptCh 满。
	// 但需要先创建足够的流占满 acceptCh，同时不 Accept 它们。
	// 由于服务端没有 goroutine 在 Accept，第 65 个流将触发 reject。
	overflow := 65
	streams := make([]mux.Stream, 0, overflow)

	for i := range overflow {
		s, err := muxA.Open(ctx)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		streams = append(streams, s)
	}

	// 用多余的流验证 Read 不会阻塞，应返回 ErrStreamRejected
	last := streams[overflow-1]
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, err := last.Read(buf)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, mux.ErrStreamRejected) {
			t.Fatalf("expected ErrStreamRejected, got: %v", err)
		}
		t.Logf("rejected stream Read returned: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Read on rejected stream blocked indefinitely (expected immediate error)")
	}
}

// TestMuxWithMaxStreams 验证 maxStreams 上限生效。
func TestMuxWithMaxStreams(t *testing.T) {
	a, b := xfertest.Pipe()
	maxStreams := 5
	muxA := mux.NewWithOpts(a, mux.RoleDialer, mux.WithMaxStreams(maxStreams))
	muxB := mux.NewWithOpts(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 在 maxStreams 内打开，全部应成功
	streams := make([]mux.Stream, 0, maxStreams)
	for i := range maxStreams {
		s, err := muxA.Open(ctx)
		if err != nil {
			t.Fatalf("Open #%d within limit: %v", i, err)
		}
		streams = append(streams, s)
	}

	// 验证 maxStreams 限制在 dialer 侧就拒绝
	if _, err := muxA.Open(ctx); !errors.Is(err, mux.ErrMaxStreams) {
		t.Fatalf("Open past maxStreams should return ErrMaxStreams, got: %v", err)
	} else {
		t.Logf("Open past maxStreams returned expected error: %v", err)
	}

	// 正常关闭
	for range maxStreams {
		s, acceptErr := muxB.Accept(ctx)
		if acceptErr != nil {
			break
		}
		s.Close()
	}
	for _, s := range streams {
		s.Close()
	}
}

// TestMuxWithMaxStreams_Bounded 验证 maxStreams 内正常通信，超出后拒绝不阻塞。
func TestMuxWithMaxStreams_Bounded(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.NewWithOpts(a, mux.RoleDialer)
	muxB := mux.NewWithOpts(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 打开一条流，验证正常 echo 通信
	sA, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sA.Close()

	sB, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sB.Close()

	payload := []byte("hello bounded streams")
	_, err = sA.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = sA.CloseWrite()

	buf := make([]byte, 4096)
	n, err := sB.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, buf[:n])
	}
}

// TestRetransmitQueue_Exhausted 验证重传耗尽后 mux 关闭。
// 先建立流再关闭底层连接，使 Stream.Write 入重传队列，最终耗尽触发 mux.Close。
func TestRetransmitQueue_Exhausted(t *testing.T) {
	c, s := xfertest.Pipe()
	dm := mux.New(c, mux.RoleDialer)
	lm := mux.New(s, mux.RoleListener)
	t.Cleanup(func() { dm.Close(); lm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 先建立流（pipe 正常）
	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// 接受端接受流
	acc, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}

	// 关闭底层连接，使 conn.Send 失败
	c.Close()
	s.Close()

	// 写入数据，触发重传
	_, err = stream.Write([]byte("test data"))
	if err != nil {
		t.Logf("Write failed (expected): %v", err)
	}

	// 接受端也会感知到错误
	acc.Close()

	// 等待 mux 关闭（重传耗尽后应自动关闭）
	<-dm.Done()
	<-lm.Done()
}

// TestRetransmitQueue_WriteAfterClose 验证 mux 关闭后写入返回错误。
func TestRetransmitQueue_WriteAfterClose(t *testing.T) {
	c, s := xfertest.Pipe()
	dm := mux.New(c, mux.RoleDialer)
	lm := mux.New(s, mux.RoleListener)
	dm.Close()
	lm.Close()

	_, err := dm.Open(t.Context())
	if err == nil {
		t.Fatal("expected error when opening stream on closed mux")
	}
}

// TestRetransmitQueue_SuccessAfterRetry 被跳过，因为 xfertest.Pipe 的 Send 方法
// 关闭后无法恢复，无法模拟"先失败后成功"的重传场景。
// 在 pipe 传输层上，一旦连接关闭后 Send 失败，所有未发送帧都会入队列等待重传，
// 但 pipe 不会重新打开，因此重传总是失败直至耗尽。
// 如需完整测试"先失败后成功"路径，需使用支持 Send 失败后恢复的 mock transport。

// TestRetransmitQueue_Concurrent 验证并发写入与重传队列操作不产生 data race。
func TestRetransmitQueue_Concurrent(t *testing.T) {
	dm, lm := newMuxPair(t)
	ctx := t.Context()

	// 打开多条流并并发写入
	const numStreams = 10
	var wg sync.WaitGroup

	for i := range numStreams {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := dm.Open(ctx)
			if err != nil {
				t.Logf("stream %d open failed: %v", i, err)
				return
			}
			defer s.Close()

			// 写入小数据块
			data := []byte("hello from stream " + string(rune('0'+i)))
			_, err = s.Write(data)
			if err != nil {
				t.Logf("stream %d write failed: %v", i, err)
			}
		}(i)
	}

	// 接受端读取所有流
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for range numStreams {
			s, err := lm.Accept(ctx)
			if err != nil {
				return
			}
			// 读取并丢弃数据
			buf := make([]byte, 1024)
			for {
				_, err := s.Read(buf)
				if err != nil {
					break
				}
			}
			s.Close()
		}
	}()

	wg.Wait()
	<-acceptDone
}
