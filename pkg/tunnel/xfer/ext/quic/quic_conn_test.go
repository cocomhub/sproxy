// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
)

// TestRegistry 验证 QUIC 传输已注册到 xfer.TransportRegistry。
func TestRegistry(t *testing.T) {
	tr := xfer.Get("quic")
	if tr == nil {
		t.Fatal("expected quic transport in registry")
	}
	if tr.Name != "quic" {
		t.Fatalf("expected name 'quic', got %q", tr.Name)
	}
	if tr.Dial == nil {
		t.Fatal("expected non-nil Dial function")
	}
	if tr.Listen == nil {
		t.Fatal("expected non-nil Listen function")
	}
}

// TestDialTLSConfig_Valid 从外部测试 DialTLSConfig 正常返回。
func TestDialTLSConfig_Valid(t *testing.T) {
	conf, err := quic.DialTLSConfig("127.0.0.1:9000")
	if err != nil {
		t.Fatal(err)
	}
	if conf.ServerName != "127.0.0.1" {
		t.Fatalf("expected ServerName 127.0.0.1, got %q", conf.ServerName)
	}
	if len(conf.NextProtos) == 0 || conf.NextProtos[0] != "sproxy-quic" {
		t.Fatalf("expected NextProtos [\"sproxy-quic\"], got %v", conf.NextProtos)
	}
}

// TestDialTLSConfig_InvalidAddr 从外部测试 DialTLSConfig 对无效地址返回错误。
func TestDialTLSConfig_InvalidAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"missing port", "127.0.0.1"},
		{"empty address", ""},
		{"malformed brackets", "[::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := quic.DialTLSConfig(tt.addr)
			if err == nil {
				t.Fatal("expected error for invalid address")
			}
		})
	}
}

func quicNetSupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
}

// newQUICConnPair 启动 Listener、建立一次 Dial 握手，返回 client 与 server 连接。
// listener 会额外返回，供测试者在 cleanup 中关闭。
func newQUICConnPair(t *testing.T) (client, server xfer.Conn, listener xfer.Listener, cleanup func()) {
	t.Helper()
	quicNetSupported(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ln, err := quic.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	qln, ok := ln.(*quic.QuicListener)
	if !ok {
		t.Fatalf("expected *quic.QuicListener, got %T", ln)
	}
	addr := qln.Addr()

	type acceptResult struct {
		conn xfer.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		conn, aerr := ln.Accept(ctx)
		acceptCh <- acceptResult{conn, aerr}
	}()

	clientConn, err := quic.Dial(ctx, addr)
	if err != nil {
		ln.Close()
		t.Fatalf("dial failed: %v", err)
	}

	result := <-acceptCh
	if result.err != nil {
		clientConn.Close()
		ln.Close()
		t.Fatalf("accept failed: %v", result.err)
	}

	cleanup = func() {
		clientConn.Close()
		result.conn.Close()
		ln.Close()
	}
	return clientConn, result.conn, ln, cleanup
}

// TestDial_Unreachable 验证向不可达地址 Dial 返回错误。
func TestDial_Unreachable(t *testing.T) {
	quicNetSupported(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := quic.Dial(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected error dialing unreachable address")
	}
}

// TestDial_InvalidAddr 验证向无效地址格式 Dial 返回错误。
func TestDial_InvalidAddr(t *testing.T) {
	quicNetSupported(t)
	ctx := context.Background()
	_, err := quic.Dial(ctx, "invalid-addr-format")
	if err == nil {
		t.Fatal("expected error for invalid address format")
	}
}

// TestSendAfterClose 验证连接关闭后 Send 返回错误。
func TestSendAfterClose(t *testing.T) {
	client, _, _, cleanup := newQUICConnPair(t)
	defer cleanup()

	// 先正常发送一条确认连接可用
	if err := client.Send(context.Background(), []byte("ping")); err != nil {
		t.Fatalf("initial send failed: %v", err)
	}

	// 关闭连接后发送应返回错误
	client.Close()
	err := client.Send(context.Background(), []byte("after close"))
	if err == nil {
		t.Fatal("expected error when sending after close")
	}
}

// TestIdempotentClose 验证多次 Close 不会 panic 且均返回 nil。
func TestIdempotentClose(t *testing.T) {
	client, server, ln, cleanup := newQUICConnPair(t)
	defer cleanup()
	_ = server
	_ = ln

	// 第一次 Close
	if err := client.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	// 第二次 Close（幂等）
	if err := client.Close(); err != nil {
		t.Fatalf("second close should be idempotent, got: %v", err)
	}
	// 第三次 Close
	if err := client.Close(); err != nil {
		t.Fatalf("third close should be idempotent, got: %v", err)
	}
}

// TestListener_AcceptAfterClose 验证 Listener 关闭后 Accept 返回错误。
func TestListener_AcceptAfterClose(t *testing.T) {
	quicNetSupported(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := quic.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// 关闭 listener
	ln.Close()

	// Accept 在关闭后应返回错误
	_, err = ln.Accept(ctx)
	if err == nil {
		t.Fatal("expected error when accepting on closed listener")
	}
}
