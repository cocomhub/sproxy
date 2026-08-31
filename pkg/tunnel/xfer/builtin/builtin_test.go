// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package builtin_test

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	builtin "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 命名导入：注册内置传输层（tcp/tcp+tls）并暴露配置桥
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

// TestBuiltinSetDefaultTLSConfig 验证 SetDefaultTLSConfig 桥：外部调用方可经 builtin
// 设置 tcp+tls 传输的默认 TLS 配置（绕过 internal 可见性约束），设置后 tcp+tls 变体
// Listen 可用；未设置时明确报错（fail-closed，防无凭据明文承载）。
func TestBuiltinSetDefaultTLSConfig(t *testing.T) {
	tp := xfer.Get("tcp+tls")
	if tp == nil {
		t.Fatal("tcp+tls transport not registered after importing xfer/builtin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 未设置 → Listen 报错（fail-closed）。
	builtin.SetDefaultTLSConfig(nil)
	if _, err := tp.Listen(ctx, "127.0.0.1:0"); err == nil {
		t.Fatal("未设置默认 TLS 配置时 tcp+tls Listen 应报错")
	}

	// 设置自签证书配置 → Listen 成功。
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })
	builtin.SetDefaultTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})

	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp+tls Listen（经 builtin 设置默认配置后）: %v", err)
	}
	defer ln.Close()
}
