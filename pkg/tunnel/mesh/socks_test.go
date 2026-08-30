// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// startLocalEcho 起本地 TCP echo，返回地址。
func startLocalEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(cc net.Conn) {
				defer cc.Close()
				_, _ = io.Copy(cc, cc)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestServeLocalSocks（mesh node --socks）：本地 SOCKS5 出口——CONNECT 目标由节点
// 本机拨号，官方 x/net/proxy 客户端数据往返。
func TestServeLocalSocks(t *testing.T) {
	echoAddr := startLocalEcho(t)
	ln, ss, err := newLocalSocks("127.0.0.1:0", "", "", testMDNSLogger())
	if err != nil {
		t.Fatalf("newLocalSocks: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ss.Serve(ctx, ln) }()

	dialer, err := proxy.SOCKS5("tcp", ln.Addr().String(), nil, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	conn, err := dialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("CONNECT 失败: %v", err)
	}
	defer conn.Close()
	if _, werr := conn.Write([]byte("local-socks")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 32)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf[:n]) != "local-socks" {
		t.Fatalf("echo = %q, want local-socks", buf[:n])
	}
}

// TestServeLocalSocks_Auth（安全审查）：配置 RFC 1929 认证后，正确凭据可用、错误
// 凭据被拒（防未授权使用本节点作代理）。
func TestServeLocalSocks_Auth(t *testing.T) {
	echoAddr := startLocalEcho(t)
	ln, ss, err := newLocalSocks("127.0.0.1:0", "user", "pass", testMDNSLogger())
	if err != nil {
		t.Fatalf("newLocalSocks: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ss.Serve(ctx, ln) }()

	// 错误凭据 → CONNECT 失败。
	badDialer, _ := proxy.SOCKS5("tcp", ln.Addr().String(), &proxy.Auth{User: "user", Password: "wrong"}, nil)
	if conn, derr := badDialer.Dial("tcp", echoAddr); derr == nil {
		_ = conn.Close()
		t.Fatal("错误凭据应导致 CONNECT 失败")
	}
	// 正确凭据 → 数据往返。
	goodDialer, _ := proxy.SOCKS5("tcp", ln.Addr().String(), &proxy.Auth{User: "user", Password: "pass"}, nil)
	conn, derr := goodDialer.Dial("tcp", echoAddr)
	if derr != nil {
		t.Fatalf("正确凭据 CONNECT 失败: %v", derr)
	}
	defer conn.Close()
	if _, werr := conn.Write([]byte("auth-socks")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 32)
	n, rerr := conn.Read(buf)
	if rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(buf[:n]) != "auth-socks" {
		t.Fatalf("echo = %q, want auth-socks", buf[:n])
	}
}
