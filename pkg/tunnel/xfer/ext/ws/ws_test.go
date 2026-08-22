// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestWS(t *testing.T) {
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "ws",
		Dial:   ws.Dial,
		Listen: ws.Listen,
	})
}

// TestWSConcurrentHandlerNodeClose 验证 HandlerNode.Close 并发安全：
// 并发调用 Close 不得触发 close of closed channel panic（I15）。
func TestWSConcurrentHandlerNodeClose(t *testing.T) {
	n := ws.NewHandlerNode()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// TestWSFlushAfterClose 验证连接关闭后 Flush 立即返回 ErrConnClosed 而非悬挂（I17）。
func TestWSFlushAfterClose(t *testing.T) {
	client, _, cleanup := newWSPair(t)
	defer cleanup()

	fl, ok := client.(xfer.Flusher)
	if !ok {
		t.Fatal("ws conn does not implement xfer.Flusher")
	}
	client.Close()

	done := make(chan error, 1)
	go func() {
		done <- fl.Flush(context.Background())
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from Flush after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Flush hung after Close")
	}
}

// TestWSMessageTooBig 验证超过 maxMessageBytes(1MiB) 的消息对端 Receive 返回错误（I19）。
func TestWSMessageTooBig(t *testing.T) {
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	// 1 MiB + 1 字节，超过 maxMessageBytes（coder 内部 +1 余量后仍超限）。
	big := make([]byte, 1<<20+1)
	recvErr := make(chan error, 1)
	go func() {
		_, err := server.Receive(context.Background())
		recvErr <- err
	}()
	if err := client.Send(context.Background(), big); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-recvErr:
		if err == nil {
			t.Fatal("expected error receiving message over 1 MiB limit")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not return error for oversized message")
	}
}

// newWSPair 建立一对 ws 连接（客户端 + 服务端），供 ws 专属测试使用。
func newWSPair(t *testing.T) (client, server xfer.Conn, cleanup func()) {
	t.Helper()
	ctx := context.Background()

	listener, err := ws.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listenerAddr(listener)

	type acceptResult struct {
		conn xfer.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		c, aerr := listener.Accept(ctx)
		acceptCh <- acceptResult{c, aerr}
	}()

	clientConn, err := ws.Dial(ctx, addr)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	res := <-acceptCh
	if res.err != nil {
		clientConn.Close()
		listener.Close()
		t.Fatal(res.err)
	}
	cleanup = func() {
		clientConn.Close()
		res.conn.Close()
		listener.Close()
	}
	return clientConn, res.conn, cleanup
}

func listenerAddr(l xfer.Listener) string {
	if a, ok := l.(interface{ Addr() net.Addr }); ok {
		return a.Addr().String()
	}
	if a, ok := l.(interface{ Addr() string }); ok {
		return a.Addr()
	}
	return ""
}
