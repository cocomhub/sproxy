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
