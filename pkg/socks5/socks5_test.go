// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package socks5

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startEcho 起一个本地 TCP echo 服务，返回地址。
func startEcho(t *testing.T) string {
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

// startSocks 起一个 SOCKS5 服务器（Dial 直连），返回监听地址与清理函数。
func startSocks(t *testing.T, dial DialFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := New(Config{Dial: dial, Logger: testLogger()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx, ln) }()
	return ln.Addr().String()
}

// TestSocks5Connect_Echo：官方 x/net/proxy SOCKS5 客户端经本服务 CONNECT 到 echo，
// 数据双向往返（握手 + CONNECT + 泵送全链路）。
func TestSocks5Connect_Echo(t *testing.T) {
	echoAddr := startEcho(t)
	socksAddr := startSocks(t, nil) // Dial 直连

	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	conn, err := dialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("经 SOCKS5 CONNECT 拨号失败: %v", err)
	}
	defer conn.Close()

	if _, werr := conn.Write([]byte("hello-socks5")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if string(buf[:n]) != "hello-socks5" {
		t.Fatalf("echo = %q, want hello-socks5", buf[:n])
	}
}

// TestSocks5Connect_Domain：ATYP=Domain（--socks5-hostname 语义），hostname 由
// Dial 侧（此处直连）解析。
func TestSocks5Connect_Domain(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()
	// 从 httptest 地址取 host:port（127.0.0.1:port）。
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())

	socksAddr := startSocks(t, nil)
	dialer, err := proxy.SOCKS5("tcp", socksAddr, nil, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	// 用域名 localhost 拨号（Dial 直连解析）。
	conn, err := dialer.Dial("tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("域名 CONNECT 失败: %v", err)
	}
	defer conn.Close()
	if _, werr := conn.Write([]byte("GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if string(buf[:n]) == "" {
		t.Fatal("未收到 HTTP 响应")
	}
}

// TestSocks5Connect_CustomDial：注入的 Dial 被 CONNECT 调用（mesh 路由解耦验证）。
func TestSocks5Connect_CustomDial(t *testing.T) {
	echoAddr := startEcho(t)
	dialCalls := make(chan string, 4)
	dial := func(_ context.Context, addr string) (net.Conn, error) {
		dialCalls <- addr
		var d net.Dialer
		return d.DialContext(context.Background(), "tcp", addr)
	}
	socksAddr := startSocks(t, dial)

	dialer, _ := proxy.SOCKS5("tcp", socksAddr, nil, nil)
	conn, err := dialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("CONNECT 失败: %v", err)
	}
	defer conn.Close()
	select {
	case got := <-dialCalls:
		if got != echoAddr {
			t.Errorf("Dial 收到 %q, want %q", got, echoAddr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Dial 未被调用")
	}
}

// TestSocks5Auth_Success（安全审查：RFC 1929 认证）：配置 Auth 后，正确凭据可
// 通过认证并 CONNECT 成功。
func TestSocks5Auth_Success(t *testing.T) {
	echoAddr := startEcho(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { _ = ln.Close() })
	s := New(Config{
		Dial:   nil,
		Auth:   func(u, p string) bool { return u == "user" && p == "pass" },
		Logger: testLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx, ln) }()

	dialer, err := proxy.SOCKS5("tcp", ln.Addr().String(), &proxy.Auth{User: "user", Password: "pass"}, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	conn, err := dialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("带认证 CONNECT 失败: %v", err)
	}
	defer conn.Close()
	if _, werr := conn.Write([]byte("auth-ok")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if string(buf[:n]) != "auth-ok" {
		t.Fatalf("echo = %q, want auth-ok", buf[:n])
	}
}

// TestSocks5Auth_Failure（安全审查）：错误凭据 → 认证失败，CONNECT 被拒。
func TestSocks5Auth_Failure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { _ = ln.Close() })
	s := New(Config{
		Auth:   func(u, p string) bool { return u == "user" && p == "pass" },
		Logger: testLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx, ln) }()

	dialer, err := proxy.SOCKS5("tcp", ln.Addr().String(), &proxy.Auth{User: "user", Password: "wrong"}, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	if conn, err := dialer.Dial("tcp", "127.0.0.1:1"); err == nil {
		_ = conn.Close()
		t.Fatal("错误凭据应导致 CONNECT 失败")
	}
}

// TestSocks5Auth_RequiresAuth（安全审查）：配置 Auth 后，客户端不提供认证方法
// （只声明无认证）→ 协商失败，连接被拒。
func TestSocks5Auth_RequiresAuth(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { _ = ln.Close() })
	s := New(Config{
		Auth:   func(u, p string) bool { return true },
		Logger: testLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Serve(ctx, ln) }()

	// 手工握手：只声明无认证。
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	var m [2]byte
	if _, err := io.ReadFull(conn, m[:]); err != nil {
		t.Fatal(err)
	}
	if m[1] != MethodNoAcceptable {
		t.Fatalf("要求认证时声明无认证应得 NoAcceptable, got %d", m[1])
	}
}

// TestSocks5RejectNonConnect：BIND/UDP-ASSOCIATE 返回「命令不支持」。
func TestSocks5RejectNonConnect(t *testing.T) {
	echoAddr := startEcho(t)
	socksAddr := startSocks(t, nil)

	// 手工构造 BIND 请求（官方 client 不发送非 CONNECT）。
	conn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 握手：声明无认证。
	if _, err := conn.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	var m [2]byte
	if _, err := io.ReadFull(conn, m[:]); err != nil {
		t.Fatal(err)
	}
	if m[0] != Version5 || m[1] != MethodNoAuth {
		t.Fatalf("方法协商应答 = %v, want [5 0]", m)
	}
	// BIND 请求：ATYP=IPv4, 目标 echoAddr 的 host:port。
	host, portStr, _ := net.SplitHostPort(echoAddr)
	port := mustPort(t, portStr)
	req := []byte{Version5, CmdBind, 0, AtypIPv4}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [4]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		t.Fatal(err)
	}
	if rep[1] != ReplyCommandNotSupported {
		t.Fatalf("BIND 应答码 = %d, want %d", rep[1], ReplyCommandNotSupported)
	}
}

// TestSocks5DialError：Dial 失败回合适应答码（此处连接拒绝 → 0x05）。
func TestSocks5DialError(t *testing.T) {
	socksAddr := startSocks(t, func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.ErrClosedPipe}
	})
	conn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{Version5, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	var m [2]byte
	_, _ = io.ReadFull(conn, m[:])
	host, portStr, _ := net.SplitHostPort("127.0.0.1:1")
	port := mustPort(t, portStr)
	req := []byte{Version5, CmdConnect, 0, AtypIPv4}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	_, _ = conn.Write(req)
	var rep [4]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		t.Fatal(err)
	}
	if rep[1] != ReplyConnectionRefused {
		t.Fatalf("拨号失败应答码 = %d, want %d", rep[1], ReplyConnectionRefused)
	}
}

// TestSocks5RejectBadVersion：非 5 版本握手被拒。
func TestSocks5RejectBadVersion(t *testing.T) {
	socksAddr := startSocks(t, nil)
	conn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 版本 4。
	if _, err := conn.Write([]byte{4, 1, MethodNoAuth}); err != nil {
		t.Fatal(err)
	}
	// 服务端应关闭连接（读返回 EOF）。
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("坏版本应被关闭连接")
	}
}

func mustPort(t *testing.T, s string) uint16 {
	t.Helper()
	var p uint16
	_, err := fmt.Sscanf(s, "%d", &p)
	if err != nil {
		t.Fatalf("非法端口 %q: %v", s, err)
	}
	return p
}
