// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestProxy 起一个注入 Dial 的 httpproxy.Server，返回监听地址。
func newTestProxy(t *testing.T, dial func(ctx context.Context, addr string) (net.Conn, error), auth func(u, p string) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(Config{Dial: dial, Auth: auth, Logger: discardLogger()})
	go func() { _ = s.Serve(t.Context(), ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// dialStub 记录拨号目标（验证「目标由 Dial 决定」），实际拨号直连 addr。
type dialStub struct {
	mu  sync.Mutex
	got []string
}

func (d *dialStub) dial(ctx context.Context, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.got = append(d.got, addr)
	d.mu.Unlock()
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

func TestForward_AbsoluteURI_GET(t *testing.T) {
	t.Parallel()
	// 目标 httptest 服务器
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Target", "ok")
		_, _ = io.WriteString(w, "hello-from-target")
	}))
	defer target.Close()

	// Dial 桩：记录目标地址并直连（证明代理把 req.URL.Host 交给 Dial）
	stub := &dialStub{}
	addr := newTestProxy(t, stub.dial, nil)

	// 代理客户端：绝对 URI 请求经代理
	proxyURL := "http://" + addr
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/path?q=1", nil)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxyURL) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-target" {
		t.Fatalf("body = %q, want hello-from-target", body)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.got) == 0 {
		t.Fatalf("Dial 未被调用（未走注入 Dial）")
	}
	if stub.got[0] != target.Listener.Addr().String() {
		t.Fatalf("Dial 目标 = %q, want %q（目标由 req.URL.Host 决定）", stub.got[0], target.Listener.Addr().String())
	}
}

func TestConnect_Tunnel_Echo(t *testing.T) {
	t.Parallel()
	// 出口 echo 服务
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cc net.Conn) { defer cc.Close(); _, _ = io.Copy(cc, cc) }(c)
		}
	}()
	// 桩：直连到 echo（验证 CONNECT 目标交给 Dial）
	stub := &dialStub{}
	addr := newTestProxy(t, stub.dial, nil)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := echoLn.Addr().String()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT 状态 = %q, want 200", status)
	}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil || line == "\r\n" {
			break
		}
	}
	if _, err := io.WriteString(conn, "ping-tunnel"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("ping-tunnel"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping-tunnel" {
		t.Fatalf("回显 = %q, want ping-tunnel", buf)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.got) == 0 || stub.got[0] != target {
		t.Fatalf("Dial 目标 = %v, want [%s]", stub.got, target)
	}
}

func TestAuth_Required_407(t *testing.T) {
	t.Parallel()
	auth := func(u, p string) bool { return u == "u" && p == "p" }
	addr := newTestProxy(t, nil, auth)

	// 未认证 CONNECT → 407
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "407") {
		t.Fatalf("未认证状态 = %q, want 407", status)
	}

	// 带正确 Basic 认证 → 认证已过，拨号失败 → 502
	addr2 := newTestProxy(t, func(ctx context.Context, target string) (net.Conn, error) {
		return nil, net.ErrClosed // 桩：拨号失败（但认证已过）
	}, auth)
	conn2, err := net.Dial("tcp", addr2)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	fmt.Fprintf(conn2, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic %s\r\n\r\n",
		base64.StdEncoding.EncodeToString([]byte("u:p")))
	br2 := bufio.NewReader(conn2)
	status2, _ := br2.ReadString('\n')
	if !strings.Contains(status2, "502") {
		t.Fatalf("认证后状态 = %q, want 502（认证已过，拨号失败）", status2)
	}
}

func TestStripHopHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Proxy-Authorization", "Basic x")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Connection", "X-Custom")
	h.Set("X-Custom", "v")
	h.Set("X-Keep", "k")
	stripHopHeaders(h)
	if h.Get("Proxy-Authorization") != "" || h.Get("Proxy-Connection") != "" {
		t.Fatalf("逐跳头未剥离: %v", h)
	}
	if h.Get("X-Custom") != "" {
		t.Fatalf("Connection 声明字段未剥离: %v", h)
	}
	if h.Get("X-Keep") != "k" {
		t.Fatalf("普通头被误删: %v", h)
	}
}

func TestForward_ProxyNil_NoLoopback(t *testing.T) {
	// 防环回：设 http_proxy 环境变量指向无效地址，请求仍不环回（转发不读系统代理环境变量）。
	// t.Setenv 禁止与 t.Parallel 共用（R18 豁免：函数体内含 t.Setenv）。
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "no-loop")
	}))
	defer target.Close()

	stub := &dialStub{}
	addr := newTestProxy(t, stub.dial, nil)
	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://" + addr) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "no-loop" {
		t.Fatalf("body = %q, want no-loop", body)
	}
}

func TestForward_NonAbsoluteURI_400(t *testing.T) {
	t.Parallel()
	addr := newTestProxy(t, nil, nil)
	// origin-form 请求（无绝对 URI）→ 400
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /path HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "400") {
		t.Fatalf("origin-form 状态 = %q, want 400", status)
	}
}
