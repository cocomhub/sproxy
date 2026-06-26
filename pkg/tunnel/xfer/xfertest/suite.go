// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// ConnFactory 是 Conn 行为测试的夹具生成函数。
type ConnFactory func(t *testing.T) (client, server xfer.Conn, cleanup func())

// ConnSuite 运行所有 Conn 接口一致性测试。
func ConnSuite(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, factory) })
	t.Run("MultipleMessages", func(t *testing.T) { testMultipleMessages(t, factory) })
	t.Run("LargePayload", func(t *testing.T) { testLargePayload(t, factory) })
	t.Run("ConcurrentSend", func(t *testing.T) { testConcurrentSend(t, factory) })
	t.Run("CloseWhileBlocking", func(t *testing.T) { testCloseWhileBlocking(t, factory) })
	t.Run("ContextCancellation", func(t *testing.T) { testContextCancellation(t, factory) })
	t.Run("OrderedDelivery", func(t *testing.T) { testOrderedDelivery(t, factory) })
	t.Run("EmptyMessage", func(t *testing.T) { testEmptyMessage(t, factory) })
	t.Run("SendAfterClose", func(t *testing.T) { testSendAfterClose(t, factory) })
}

func testRoundTrip(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	msg := []byte("hello")
	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("expected %q, got %q", msg, got)
	}
}

func testMultipleMessages(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	n := 100
	msgs := make([][]byte, n)
	for i := range n {
		msgs[i] = fmt.Appendf(nil, "msg-%d", i)
	}

	for i := range n {
		if err := client.Send(context.Background(), msgs[i]); err != nil {
			t.Fatal(err)
		}
	}

	for i := range n {
		got, err := server.Receive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, msgs[i]) {
			t.Fatalf("msg %d: expected %q, got %q", i, msgs[i], got)
		}
	}
}

func testLargePayload(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	payload := make([]byte, 1<<20) // 1 MiB
	rand.New(rand.NewSource(42)).Read(payload)

	if err := client.Send(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("large payload mismatch")
	}
}

func testConcurrentSend(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	n := 50
	var wg sync.WaitGroup
	wg.Add(n)

	for i := range n {
		msg := fmt.Appendf(nil, "concurrent-%d", i)
		go func() {
			defer wg.Done()
			if err := client.Send(ctx, msg); err != nil {
				t.Error(err)
			}
		}()
	}

	// Collect all messages on the server side.
	received := make(map[string]bool)
	for range n {
		got, err := server.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		received[string(got)] = true
	}

	wg.Wait()

	if len(received) != n {
		t.Fatalf("expected %d unique messages, got %d", n, len(received))
	}
}

func testCloseWhileBlocking(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()

	// 持续发送消息直到 channel 饱和。发送足够多以确保后台 sender
	// 即使在工作，channel 也保持满状态。
	for range 2000 {
		if err := client.Send(ctx, []byte("fill")); err != nil {
			break
		}
	}

	// 启动 goroutine，在 sendCh 满时阻塞在 select 上。
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Send(ctx, []byte("should block"))
	}()

	time.Sleep(50 * time.Millisecond)

	// Close 应中断阻塞的 Send。channel 模式下 Go select 非确定性
	// 意味着 goroutine 可能在 closeCh 关闭前成功入队，因此不强制
	// 要求 error，只确保不超时不挂起。
	client.Close()

	select {
	case <-errCh:
		// goroutine 已返回——Close 生效
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for blocked Send to return after Close")
	}

	// Drain server to close cleanly.
	for {
		_, err := server.Receive(ctx)
		if err != nil {
			break
		}
	}
}

func testContextCancellation(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, _, cleanup := factory(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 尝试多次——Go select 的非确定性意味着消息可能入 channel
	// 而非选择 ctx.Done()，尽管 ctx 已取消。
	for range 10 {
		err := client.Send(ctx, []byte("cancel"))
		if err != nil {
			return // 成功检测到取消
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected error for cancelled context after 10 attempts")
}

func testOrderedDelivery(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	n := 50
	for i := range n {
		msg := fmt.Appendf(nil, "order-%d", i)
		if err := client.Send(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}

	for i := range n {
		got, err := server.Receive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("order-%d", i)
		if string(got) != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	}
}

func testEmptyMessage(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, server, cleanup := factory(t)
	defer cleanup()

	if err := client.Send(context.Background(), []byte{}); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d bytes", len(got))
	}
}

func testSendAfterClose(t *testing.T, factory ConnFactory) {
	t.Helper()
	t.Parallel()
	client, _, cleanup := factory(t)
	defer cleanup()

	cleanup()

	err := client.Send(context.Background(), []byte("after close"))
	if err == nil {
		t.Fatal("expected error when sending after close")
	}
}
