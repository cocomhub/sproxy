// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestECDHHandshake_Roundtrip(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)

	errCh := make(chan error, 1)
	var keyA, keyB []byte
	ctx := context.Background()
	go func() {
		var err error
		keyA, err = performHandshake(ctx, muxA, true)
		errCh <- err
	}()
	keyB, err := performHandshake(ctx, muxB, false)
	if err != nil {
		t.Fatalf("listener handshake failed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("dialer handshake failed: %v", err)
	}

	if len(keyA) != 32 || len(keyB) != 32 {
		t.Fatalf("expected 32-byte session keys, got %d/%d", len(keyA), len(keyB))
	}
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("session keys do not match")
	}
}

func TestNewTunnelWithECDH(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key, _ := ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	tunA := NewTunnel(muxA, key)
	tunB := NewTunnel(muxB, key)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/pfs", strings.NewReader("pfs-test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "pfs-test" {
		t.Fatalf("expected %q, got %q", "pfs-test", string(body))
	}
	cancel()
	<-srvErr
}

func TestECDHHandshake_KeyMismatch(t *testing.T) {
	// 验证使用不同静态密钥的双方仍能通过 ECDH 协商出相同的会话密钥
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	key2, _ := ParseKey("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 服务端 goroutine 先运行 NewTunnel（监听端阻塞在 Accept）
	srvErr := make(chan error, 1)
	go func() {
		tunB := NewTunnel(muxB, key2)
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	time.Sleep(50 * time.Millisecond)

	key1, _ := ParseKey("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	tunA := NewTunnel(muxA, key1)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mismatch", strings.NewReader("mismatch-test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "mismatch-test" {
		t.Fatalf("expected %q, got %q", "mismatch-test", string(body))
	}
	cancel()
	<-srvErr
}

func TestECDHHandshake_NilKeyFallback(t *testing.T) {
	// 验证 key 为 nil 时不做 ECDH 握手，隧道正常工作
	t.Parallel()
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	tunA := NewTunnel(muxA, nil)
	tunB := NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}))
	}()
	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/nil", strings.NewReader("nil-test"))
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "nil-test" {
		t.Fatalf("expected %q, got %q", "nil-test", string(body))
	}
	cancel()
	<-srvErr
}
