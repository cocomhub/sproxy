// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xferwebrtc_test

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/xfer/webrtc"
)

func TestWebrtcRegistration(t *testing.T) {
	tp := xfer.Get("webrtc")
	if tp == nil {
		t.Fatal("webrtc transport not registered via init()")
	}
	if tp.Name != "webrtc" {
		t.Fatalf("expected name 'webrtc', got %q", tp.Name)
	}
	if tp.Dial == nil {
		t.Fatal("Dial is nil")
	}
	if tp.Listen == nil {
		t.Fatal("Listen is nil")
	}
}

func TestWebrtcDialNotImplemented(t *testing.T) {
	tp := xfer.Get("webrtc")
	if tp == nil {
		t.Fatal("webrtc transport not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tp.Dial(ctx, "stub")
	if err == nil {
		t.Fatal("expected error for unimplemented dial")
	}
}

func TestWebrtcListenNotImplemented(t *testing.T) {
	tp := xfer.Get("webrtc")
	if tp == nil {
		t.Fatal("webrtc transport not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tp.Listen(ctx, ":0")
	if err == nil {
		t.Fatal("expected error for unimplemented listen")
	}
}
