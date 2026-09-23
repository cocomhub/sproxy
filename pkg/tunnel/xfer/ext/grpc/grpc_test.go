// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package grpc

import (
	"context"
	"testing"
)

// TestGrpcRegistration verifies that the grpc transport is registered
// correctly with the local transport registry.
func TestGrpcRegistration(t *testing.T) {
	tran := Get("grpc")
	if tran == nil {
		t.Fatal("Get('grpc') returned nil; init() may not have run")
	}
	if tran.Name != "grpc" {
		t.Errorf("got transport name %q, want %q", tran.Name, "grpc")
	}
	if tran.Dial == nil {
		t.Error("Dial function is nil")
	}
	if tran.Listen == nil {
		t.Error("Listen function is nil")
	}
}

// TestDialNotImplemented verifies that Dial returns the expected "not yet implemented"
// error, which serves as a placeholder until full gRPC support is added.
func TestDialUnreachable(t *testing.T) {
	// Dial 到不可达端口 → 报错（fail-closed；不静默成功）。
	conn, err := Dial(context.Background(), "127.0.0.1:1")
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("expected an error from Dial to unreachable addr, got nil")
	}
}

// TestListenNotImplemented verifies that Listen returns the expected "not yet implemented"
// error.
func TestListenStarts(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if ln.Addr() == "" {
		t.Fatal("Addr 不应为空")
	}
}

// TestMaxConcurrentStreams_Configured 服务端并发流上限配置存在（防 DoS）。
func TestMaxConcurrentStreams_Configured(t *testing.T) {
	if maxConcurrentStreams <= 0 {
		t.Fatal("并发流上限应 > 0")
	}
	if maxConcurrentStreams > 1024 {
		t.Fatalf("上限过大（防 DoS 语义）: %d", maxConcurrentStreams)
	}
}
