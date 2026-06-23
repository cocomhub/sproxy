// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp_test

import (
	"context"
	"fmt"
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
