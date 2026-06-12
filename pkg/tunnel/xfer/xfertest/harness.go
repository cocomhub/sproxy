// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest

import (
	"context"
	"net"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// Harness 编排完整的 transport 集成测试。
type Harness struct {
	Name   string
	Dial   func(ctx context.Context, addr string) (xfer.Conn, error)
	Listen func(ctx context.Context, addr string) (xfer.Listener, error)
}

// TestHarness 运行 ConnSuite + 端到端 Listener-Dial 测试。
func TestHarness(t *testing.T, h Harness) {
	t.Run(h.Name, func(t *testing.T) {
		t.Parallel()

		connFactory := func(t *testing.T) (client, server xfer.Conn, cleanup func()) {
			t.Helper()
			ctx := context.Background()

			listener, err := h.Listen(ctx, "127.0.0.1:0")
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

			clientConn, err := h.Dial(ctx, addr)
			if err != nil {
				listener.Close()
				t.Fatal(err)
			}

			result := <-acceptCh
			if result.err != nil {
				clientConn.Close()
				listener.Close()
				t.Fatal(result.err)
			}

			cleanup = func() {
				clientConn.Close()
				result.conn.Close()
				listener.Close()
			}
			return clientConn, result.conn, cleanup
		}

		ConnSuite(t, connFactory)
	})
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
