// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package builtin_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置传输层（tcp）
)

// TestBuiltinRegistersTCP 验证 blank import xfer/builtin 后内置 TCP 传输层
// 已注册（供 hub 裸 TCP 中继与 sclient relay --transport tcp 使用）。
func TestBuiltinRegistersTCP(t *testing.T) {
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered after importing xfer/builtin")
	}
	if tp.Name != "tcp" {
		t.Fatalf("expected name 'tcp', got %q", tp.Name)
	}
	if tp.Dial == nil || tp.Listen == nil {
		t.Fatal("tcp transport Dial/Listen should be non-nil")
	}
}

// TestBuiltinTCPRoundTrip 通过 xfer/builtin 注册的 TCP 传输做一次真实消息往返，
// 证明该传输可用于 relay 注册/数据面（hub 侧 Listen / 叶子侧 Dial）。
func TestBuiltinTCPRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.(interface{ Addr() net.Addr }).Addr().String()

	var serverConn xfer.Conn
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		serverConn, err = ln.Accept(ctx)
	}()
	clientConn, derr := tp.Dial(ctx, addr)
	if derr != nil {
		t.Fatal(derr)
	}
	defer clientConn.Close()
	<-acceptDone
	if err != nil {
		t.Fatal(err)
	}
	if serverConn == nil {
		t.Fatal("expected accepted server conn")
	}
	defer serverConn.Close()

	msg := []byte("builtin-tcp-ping")
	if serr := clientConn.Send(ctx, msg); serr != nil {
		t.Fatal(serr)
	}
	got, rerr := serverConn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, got)
	}
}
