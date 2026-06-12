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
func TestDialNotImplemented(t *testing.T) {
	conn, err := Dial(context.Background(), "localhost:50051")
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("expected an error from Dial, got nil")
	}
	if err.Error() != "grpc: not yet implemented, use ws instead" {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

// TestListenNotImplemented verifies that Listen returns the expected "not yet implemented"
// error.
func TestListenNotImplemented(t *testing.T) {
	lis, err := Listen(context.Background(), ":50051")
	if err == nil {
		if lis != nil {
			lis.Close()
		}
		t.Fatal("expected an error from Listen, got nil")
	}
	if err.Error() != "grpc: not yet implemented, use ws instead" {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}
