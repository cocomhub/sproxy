// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/internal/tcp" // 注册 tcp transport（init）
)

// TestTcpConnRoundTrip 测试 TCP 传输的基本消息往返。
func TestTcpConnRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
		return
	}

	listener, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	tcpLn, ok := listener.(interface{ Addr() net.Addr })
	if !ok {
		t.Fatal("listener does not implement Addr()")
	}
	addr := tcpLn.Addr().String()

	var serverConn xfer.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		serverConn, acceptErr = listener.Accept(ctx)
	})
	time.Sleep(50 * time.Millisecond)

	clientConn, err := tp.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	if serverConn == nil {
		t.Fatal("expected server conn")
	}
	defer serverConn.Close()

	msg := []byte("hello tcp")
	if err = clientConn.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	received, err := serverConn.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(received) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, received)
	}

	reply := []byte("reply")
	if err = serverConn.Send(ctx, reply); err != nil {
		t.Fatal(err)
	}
	received, err = clientConn.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(received) != string(reply) {
		t.Fatalf("expected %q, got %q", reply, received)
	}
}

func TestTcpLargePayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	listener, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	tcpLn, ok := listener.(interface{ Addr() net.Addr })
	if !ok {
		t.Fatal("listener does not implement Addr()")
	}
	addr := tcpLn.Addr().String()

	var serverConn xfer.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		serverConn, acceptErr = listener.Accept(ctx)
	})
	time.Sleep(50 * time.Millisecond)

	clientConn, err := tp.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	defer serverConn.Close()

	// 1MB payload
	payload := make([]byte, 1048576)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	if err = clientConn.Send(ctx, payload); err != nil {
		t.Fatal(err)
	}
	received, err := serverConn.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != len(payload) {
		t.Fatalf("expected %d bytes, got %d", len(payload), len(received))
	}
	for i := range payload {
		if received[i] != payload[i] {
			t.Fatalf("byte mismatch at %d", i)
		}
	}
}

func TestTcpRegistration(t *testing.T) {
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered via init()")
		return
	}
	if tp.Name != "tcp" {
		t.Fatalf("expected name 'tcp', got %q", tp.Name)
	}
	if tp.Dial == nil {
		t.Fatal("Dial is nil")
	}
	if tp.Listen == nil {
		t.Fatal("Listen is nil")
	}
}

func TestTcpMultipleMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	listener, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	tcpLn, ok := listener.(interface{ Addr() net.Addr })
	if !ok {
		t.Fatal("listener does not implement Addr()")
	}
	addr := tcpLn.Addr().String()

	var serverConn xfer.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		serverConn, acceptErr = listener.Accept(ctx)
	})
	time.Sleep(50 * time.Millisecond)

	clientConn, err := tp.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	defer serverConn.Close()

	for i := range 10 {
		msg := fmt.Appendf(nil, "msg-%d", i)
		if err = clientConn.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		received, err := serverConn.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(received) != string(msg) {
			t.Fatalf("expected %q, got %q", msg, received)
		}
	}
}

// newTCPPair 建立一对 TCP 传输连接（listener 侧 Accept 的 serverConn 与
// 客户端侧 clientConn），返回后由调用方负责关闭。
func newTCPPair(t *testing.T, ctx context.Context) (clientConn, serverConn xfer.Conn, addr string) {
	t.Helper()
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr = ln.(interface{ Addr() net.Addr }).Addr().String()

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		serverConn, err = ln.Accept(ctx)
	}()
	clientConn, derr := tp.Dial(ctx, addr)
	if derr != nil {
		t.Fatal(derr)
	}
	t.Cleanup(func() { _ = clientConn.Close() })
	<-acceptDone
	if err != nil {
		t.Fatal(err)
	}
	if serverConn == nil {
		t.Fatal("expected accepted server conn")
	}
	t.Cleanup(func() { _ = serverConn.Close() })
	return clientConn, serverConn, addr
}

// TestTcpReceive_RespectsCtxTimeout 验证 Receive 尊重 ctx deadline：对端连接后
// 不发送任何数据，Receive 应在 ctx 超时后返回错误而非无限阻塞。
// hub 裸 TCP 中继下若 RegisterFrame 超时不能被中断，会永久占住连接槽与
// goroutine（DoS 面），因此该行为是硬约束。
func TestTcpReceive_RespectsCtxTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, serverConn, _ := newTCPPair(t, ctx)

	rctx, rcancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer rcancel()
	start := time.Now()
	_, err := serverConn.Receive(rctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Receive to return an error after ctx timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) && !isNetTimeout(err) {
		t.Fatalf("expected deadline/timeout error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Receive blocked too long after ctx timeout: %v", elapsed)
	}
}

// TestTcpReceive_CloseUnblocksBlockedReceive 验证阻塞中的 Receive 在连接关闭后
// 及时返回（mux readLoop 用 cancel-only ctx + conn.Close 解除阻塞的收尾路径）。
func TestTcpReceive_CloseUnblocksBlockedReceive(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, serverConn, _ := newTCPPair(t, ctx)

	done := make(chan error, 1)
	go func() {
		_, err := serverConn.Receive(ctx)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if cerr := serverConn.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Receive did not return promptly after conn close: %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Receive blocked forever after conn close")
	}
}

// TestTcpReceive_RejectsOversizedMessage 验证 Receive 对超大长度前缀消息返回错误
// 而非 OOM（恶意对端可发 0xFFFFFFFF 长度前缀触发巨型分配）。上限与 WS 传输对齐
// （1 MiB），mux 帧（8B 头 + 64 KiB 负载）远小于该值。
func TestTcpReceive_RejectsOversizedMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.(interface{ Addr() net.Addr }).Addr().String()

	var serverConn xfer.Conn
	var acceptErr error
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		serverConn, acceptErr = ln.Accept(ctx)
	}()
	// 客户端用裸 net.Dial 连接，直接写 4B 超大长度前缀（不写 body），
	// 触发服务端长度检查而非分配。
	raw, derr := net.Dial("tcp", addr)
	if derr != nil {
		t.Fatal(derr)
	}
	defer raw.Close()
	<-acceptDone
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	defer serverConn.Close()

	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, 0xFFFFFFFF)
	if _, werr := raw.Write(frame); werr != nil {
		t.Fatal(werr)
	}

	if _, rerr := serverConn.Receive(ctx); rerr == nil {
		t.Fatal("expected error for oversized length prefix")
	}
}

// isNetTimeout 报告 err 是否为 net 超时（*net.OpError Timeout()）。
func isNetTimeout(err error) bool {
	var ne *net.OpError
	return errors.As(err, &ne) && ne.Timeout()
}
