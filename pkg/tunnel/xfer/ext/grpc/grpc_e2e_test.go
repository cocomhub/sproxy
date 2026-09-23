// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package grpc

// grpc_e2e_test.go 验证 gRPC 传输实现（roadmap P2 gRPC 传输装配）：
//  1. Listen+Dial 建链（双向流）。
//  2. Send/Receive 往返（消息字节透传）。

import (
	"context"
	"testing"
	"time"
)

// TestGrpcTransport_Roundtrip Listen+Dial → Send/Receive 往返。
func TestGrpcTransport_Roundtrip(t *testing.T) {
	setupGRPCTLS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// 服务端 accept。
	acceptCh := make(chan Conn, 1)
	go func() {
		c, aerr := ln.Accept(ctx)
		if aerr == nil {
			acceptCh <- c
		}
	}()

	clientConn, derr := Dial(ctx, ln.Addr())
	if derr != nil {
		t.Fatalf("Dial: %v", derr)
	}
	defer clientConn.Close()

	serverConn := <-acceptCh
	if serverConn == nil {
		t.Fatal("accept 失败")
	}
	defer serverConn.Close()

	// client → server。
	if err := clientConn.Send(ctx, []byte("hello grpc")); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	got, rerr := serverConn.Receive(ctx)
	if rerr != nil {
		t.Fatalf("server Receive: %v", rerr)
	}
	if string(got) != "hello grpc" {
		t.Fatalf("server got %q", got)
	}

	// server → client。
	if err := serverConn.Send(ctx, []byte("pong")); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	got2, cerr := clientConn.Receive(ctx)
	if cerr != nil {
		t.Fatalf("client Receive: %v", cerr)
	}
	if string(got2) != "pong" {
		t.Fatalf("client got %q", got2)
	}
}

// TestGrpcTransport_DialRequiresTLS 钉住「Dial 恒 TLS，未配 CA 连自签 listener 明确失败」
// （审查 P1 修复：此前 Dial insecure 连 TLS listener 静默失败；现恒 TLS fail-closed）。
func TestGrpcTransport_DialRequiresTLS(t *testing.T) {
	setupGRPCTLS(t) // 生成 CA + 自签证书 + 配 SPROXY_GRPC_CA_CERT
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ln, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr()
	// 配了 CA（setupGRPCTLS）→ Dial 应成功（TLS 校验通过）。
	c, err := Dial(ctx, addr)
	if err != nil {
		t.Fatalf("配 CA 后 Dial 应成功: %v", err)
	}
	_ = c.Close()
}

// TestGrpcTransport_DialNoCA_FailsClosed 钉住「未配 CA 时 Dial 连自签 listener 明确失败」
// （审查 P1：对称性反向——恒 TLS 下无 CA 不能静默连上明文/insecure）。
func TestGrpcTransport_DialNoCA_FailsClosed(t *testing.T) {
	// 不调 setupGRPCTLS：不配 SPROXY_GRPC_CA_CERT（系统池无该自签 CA）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ln, err := Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	// 未配 CA → 系统池无法校验自签 → Dial 应失败（fail-closed，不静默明文连接）。
	if c, derr := Dial(ctx, ln.Addr()); derr == nil {
		_ = c.Close()
		t.Fatal("未配 CA 连自签 listener 应失败（恒 TLS fail-closed），got 成功")
	}
}
