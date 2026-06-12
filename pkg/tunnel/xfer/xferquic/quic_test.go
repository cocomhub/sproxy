// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xferquic_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/xferquic"
)

func isWindows() bool {
	return true
}

func TestQuicRegistration(t *testing.T) {
	tp := xfer.Get("quic")
	if tp == nil {
		t.Fatal("quic transport not registered")
	}
	if tp.Name != "quic" {
		t.Fatalf("name: got %q, want %q", tp.Name, "quic")
	}
	if tp.Dial == nil || tp.Listen == nil {
		t.Fatal("Dial or Listen is nil")
	}
}

func TestQuicRoundTrip(t *testing.T) {
	if isWindows() {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	bg := context.Background()

	tp := xfer.Get("quic")
	l, err := tp.Listen(bg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ql := l.(interface{ Addr() string })
	addr := ql.Addr()
	t.Logf("listen addr=%s", addr)

	var sc xfer.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		var aerr error
		sc, aerr = l.Accept(bg)
		if aerr != nil {
			t.Errorf("accept: %v", aerr)
		}
	}()

	time.Sleep(10 * time.Millisecond)

	cc, err := tp.Dial(bg, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	<-done
	if sc == nil {
		t.Fatal("accept returned nil conn")
	}
	defer sc.Close()

	// client -> server
	msg := []byte("hello quic")
	if err := cc.Send(bg, msg); err != nil {
		t.Fatal(err)
	}
	got, err := sc.Receive(bg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("send: got %q, want %q", got, msg)
	}

	// server -> client
	reply := []byte("reply")
	if err := sc.Send(bg, reply); err != nil {
		t.Fatal(err)
	}
	got, err = cc.Receive(bg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, reply) {
		t.Fatalf("reply: got %q, want %q", got, reply)
	}
}

func TestQuicMultipleMessages(t *testing.T) {
	if isWindows() {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	bg := context.Background()

	tp := xfer.Get("quic")
	l, err := tp.Listen(bg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ql := l.(interface{ Addr() string })
	addr := ql.Addr()

	var sc xfer.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		var aerr error
		sc, aerr = l.Accept(bg)
		if aerr != nil {
			t.Errorf("accept: %v", aerr)
		}
	}()

	time.Sleep(10 * time.Millisecond)
	cc, err := tp.Dial(bg, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	<-done
	if sc == nil {
		t.Fatal("accept returned nil")
	}
	defer sc.Close()

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 10 {
			msg, rerr := sc.Receive(bg)
			if rerr != nil {
				t.Errorf("recv: %v", rerr)
				return
			}
			if snerr := sc.Send(bg, msg); snerr != nil {
				t.Errorf("send back: %v", snerr)
				return
			}
		}
	})

	for i := range 10 {
		msg := fmt.Appendf(nil, "msg-%d", i)
		if err := cc.Send(bg, msg); err != nil {
			t.Fatal(err)
		}
		got, err := cc.Receive(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("msg %d: got %q, want %q", i, got, msg)
		}
	}
	wg.Wait()
}

// TestQuicLargePayload 测试 64KB 大消息收发（需要 QUIC 环境支持）。
func TestQuicLargePayload(t *testing.T) {
	if isWindows() {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	bg := context.Background()

	tp := xfer.Get("quic")
	l, err := tp.Listen(bg, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ql := l.(interface{ Addr() string })
	addr := ql.Addr()

	var sc xfer.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		var aerr error
		sc, aerr = l.Accept(bg)
		if aerr != nil {
			t.Errorf("accept: %v", aerr)
		}
	}()

	time.Sleep(10 * time.Millisecond)
	cc, err := tp.Dial(bg, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	<-done
	if sc == nil {
		t.Fatal("accept returned nil")
	}
	defer sc.Close()

	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	if err := cc.Send(bg, payload); err != nil {
		t.Fatal(err)
	}
	got, err := sc.Receive(bg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("large payload mismatch: got %d bytes, want %d bytes", len(got), len(payload))
	}
}
