// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// testLogger 返回丢弃日志的 logger。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDialAllowed(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		// IP 直写：回环/私有/链路本地/多播/未指定 → 拒绝
		{"loopback", "127.0.0.1:22", false},
		{"loopback-v6", "[::1]:22", false},
		{"private-10", "10.0.0.5:8080", false},
		{"private-192", "192.168.1.100:22", false},
		{"private-172", "172.16.3.9:443", false},
		{"link-local", "169.254.10.20:80", false},
		{"multicast", "224.0.0.1:80", false},
		{"unspecified", "0.0.0.0:80", false},
		// 公网 IP → 允许
		{"public-ip", "8.8.8.8:53", true},
		{"public-ip-v6", "[2606:4700:4700::1111]:443", true},
		// 主机名：解析后按 IP 校验；.invalid 为 RFC 2606 保留、必然解析失败 → 拒绝
		{"hostname-unresolvable", "no-such-host.invalid:22", false},
		// 非法输入 → 拒绝
		{"bad-no-port", "127.0.0.1", false},
		{"bad-garbage", ":::", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := DialAllowed(tc.addr)
			if ok != tc.want {
				t.Fatalf("DialAllowed(%q) ok = %v, want %v", tc.addr, ok, tc.want)
			}
		})
	}
}

// TestDialAllowed_ResolvedAddress 验证主机名放行时返回解析后的 IP:port（防 rebinding TOCTOU）。
func TestDialAllowed_ResolvedAddress(t *testing.T) {
	resolved, ok := DialAllowed("8.8.8.8:53")
	if !ok {
		t.Fatal("expected public IP allowed")
	}
	if resolved != "8.8.8.8:53" {
		t.Fatalf("expected IP:port passthrough, got %q", resolved)
	}
}

func TestNewDialPolicy(t *testing.T) {
	// 默认（无白名单）等价 DialAllowed：私网拒绝
	def := NewDialPolicy(nil)
	if _, ok := def("192.168.1.10:22"); ok {
		t.Fatal("default policy should reject private")
	}
	if _, ok := def("8.8.8.8:53"); !ok {
		t.Fatal("default policy should allow public")
	}

	// 显式白名单：放行内网网段
	withCidr := NewDialPolicy([]string{"192.168.0.0/16", "10.0.0.0/8"})
	if _, ok := withCidr("192.168.1.10:22"); !ok {
		t.Fatal("policy with cidr should allow 192.168.1.10")
	}
	if _, ok := withCidr("10.1.2.3:8080"); !ok {
		t.Fatal("policy with cidr should allow 10.x")
	}
	// 白名单之外的内网仍拒绝
	if _, ok := withCidr("172.16.0.5:80"); ok {
		t.Fatal("policy with cidr should reject non-whitelisted private 172.x")
	}
	// 公网仍放行
	if _, ok := withCidr("8.8.8.8:53"); !ok {
		t.Fatal("policy with cidr should still allow public")
	}
	// 非法输入拒绝
	if _, ok := withCidr("no-port"); ok {
		t.Fatal("invalid addr should be rejected")
	}
}

// TestNewServiceDialPolicy 验证出口拨号策略对节点自身宣告的服务地址做精确放行，
// 其余回落既有 NewDialPolicy 逻辑（公网 + 白名单 CIDR）。
func TestNewServiceDialPolicy(t *testing.T) {
	svcAddrs := []string{"127.0.0.1:10022", "localhost:10022", "10.0.0.5:22"}
	policy := NewServiceDialPolicy(nil, svcAddrs)

	tests := []struct {
		name string
		addr string
		want bool
	}{
		// 精确命中宣告地址（IP / 主机名 / 私网 IP）→ 放行，返回原地址
		{"announced-loopback", "127.0.0.1:10022", true},
		{"announced-hostname", "localhost:10022", true},
		{"announced-private", "10.0.0.5:22", true},
		// 未宣告的 loopback（同 IP 不同端口）→ 回落 base 拒绝
		{"loopback-other-port", "127.0.0.1:10023", false},
		{"loopback-not-announced", "127.0.0.1:22", false},
		// 未宣告的私有地址 → 拒绝
		{"private-not-announced", "10.0.0.6:22", false},
		{"private-other", "192.168.1.10:22", false},
		// 公网地址 → 回落 base 放行
		{"public", "8.8.8.8:53", true},
		// 畸形地址（无端口）→ 拒绝，即使字符串在宣告列表里
		{"announced-no-port", "127.0.0.1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := policy(tc.addr)
			if ok != tc.want {
				t.Fatalf("policy(%q) ok = %v, want %v", tc.addr, ok, tc.want)
			}
			if tc.want && got != tc.addr {
				t.Fatalf("policy(%q) resolved = %q, want passthrough %q", tc.addr, got, tc.addr)
			}
		})
	}
}

// TestNewServiceDialPolicy_WithCIDR 验证服务地址白名单与 CIDR 白名单叠加：
// 宣告地址、CIDR 内地址放行；CIDR 外私有地址拒绝。
func TestNewServiceDialPolicy_WithCIDR(t *testing.T) {
	policy := NewServiceDialPolicy([]string{"192.168.0.0/16"}, []string{"127.0.0.1:10022"})
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:10022", true},  // 宣告地址
		{"192.168.5.100:22", true}, // CIDR 白名单
		{"8.8.8.8:53", true},       // 公网
		{"10.0.0.5:22", false},     // CIDR 外的私有
		{"127.0.0.1:9999", false},  // 未宣告的 loopback
	}
	for _, tc := range cases {
		if _, ok := policy(tc.addr); ok != tc.want {
			t.Fatalf("policy(%q) ok = %v, want %v", tc.addr, ok, tc.want)
		}
	}
}

// TestNewServiceDialPolicy_NoServiceAddrs 验证 serviceAddrs 为空时等价默认策略：
// 私网拒绝、公网放行。
func TestNewServiceDialPolicy_NoServiceAddrs(t *testing.T) {
	policy := NewServiceDialPolicy(nil, nil)
	if _, ok := policy("192.168.1.10:22"); ok {
		t.Fatal("empty serviceAddrs policy should reject private")
	}
	if _, ok := policy("8.8.8.8:53"); !ok {
		t.Fatal("empty serviceAddrs policy should allow public")
	}
}

// TestNewDialPolicy_ResolvedCIDR 验证白名单命中时返回解析后的 IP:port。
func TestNewDialPolicy_ResolvedCIDR(t *testing.T) {
	withCidr := NewDialPolicy([]string{"192.168.0.0/16"})
	resolved, ok := withCidr("192.168.5.100:22")
	if !ok {
		t.Fatal("expected CIDR-whitelisted addr allowed")
	}
	if resolved != "192.168.5.100:22" {
		t.Fatalf("expected resolved IP:port, got %q", resolved)
	}
}

// TestServeHTTP_RelaysToLocal 验证 serveHTTP 分支：把 [4B len][tunnel.Request]
// 元数据帧解析后转发到本地 HTTP 服务，并把响应 metadata+body 写回流。
func TestServeHTTP_RelaysToLocal(t *testing.T) {
	// 本地 HTTP 服务（验证转发目标）
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("X-Backend", "relay")
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	// 双端流：serveHTTP 端 + 模拟调用方端
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	// serveHTTP 端：开一条流处理。注意不 defer Close——由测试读完响应后再关闭，
	// 避免 serveHTTP 返回时关流与客户端读 body 竞争（-race 下必现）。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	streamCh := make(chan mux.Stream, 1)
	go func() {
		stream, aerr := serverMux.Accept(ctx)
		if aerr != nil {
			serverDone <- aerr
			return
		}
		streamCh <- stream
		httpClient := &http.Client{Timeout: 5 * time.Second}
		serveHTTP(ctx, stream, backend.URL, tunnel.Request{Method: http.MethodGet, URL: "/relayed"}, httpClient, testLogger())
		serverDone <- nil
	}()

	// 调用方端：构造 HTTP 请求 metadata 帧，写入流并读响应
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	reqMeta, _ := json.Marshal(tunnel.Request{Method: http.MethodGet, URL: "/relayed"})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(reqMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(reqMeta); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 读响应 metadata（[4B len][json]）。serveHTTP 写 meta 先于 body，
	// 客户端应能读到 meta（此时 serveHTTP 可能仍在写 body 或已返回）。
	metaLenBuf := make([]byte, 4)
	if _, rerr := io.ReadFull(clientStream, metaLenBuf); rerr != nil {
		t.Fatal(rerr)
	}
	metaLen := binary.BigEndian.Uint32(metaLenBuf)
	metaRaw := make([]byte, metaLen)
	if _, rerr := io.ReadFull(clientStream, metaRaw); rerr != nil {
		t.Fatal(rerr)
	}
	var respMeta tunnel.Response
	if err := json.Unmarshal(metaRaw, &respMeta); err != nil {
		t.Fatal(err)
	}
	if respMeta.Status != http.StatusOK {
		t.Fatalf("expected 200, got %d", respMeta.Status)
	}
	if respMeta.Headers.Get("X-Backend") != "relay" {
		t.Fatalf("expected X-Backend header relayed, got %q", respMeta.Headers.Get("X-Backend"))
	}
	// 读 body（固定长度，服务端不关流所以不能 ReadAll 等 EOF）
	body := make([]byte, len("backend-ok"))
	if _, rerr := io.ReadFull(clientStream, body); rerr != nil {
		t.Fatalf("read body: %v", rerr)
	}
	if string(body) != "backend-ok" {
		t.Fatalf("expected body backend-ok, got %q", body)
	}
	if gotPath != "/relayed" {
		t.Fatalf("expected backend path /relayed, got %q", gotPath)
	}

	// 确认 serveHTTP 端正常完成，然后关闭服务端流
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("serveHTTP 端失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serveHTTP 未完成")
	}
	select {
	case s := <-streamCh:
		_ = s.Close()
	default:
	}
}

// TestServeHTTP_BadMeta 验证非法 metadata 不 panic 且关闭流。
func TestServeHTTP_BadMeta(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 写入垃圾 metadata（长度 5，内容不是 JSON）
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 5)
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write([]byte("12345"))
	_ = clientStream.CloseWrite()

	// serveHTTP 端应正常返回（无 panic）
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		stream, aerr := serverMux.Accept(ctx)
		if aerr != nil {
			return
		}
		defer stream.Close()
		httpClient := &http.Client{Timeout: 5 * time.Second}
		serveHTTP(ctx, stream, "http://127.0.0.1:1", tunnel.Request{Method: "GET", URL: "/x"}, httpClient, testLogger())
	}()
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("serveHTTP 应返回而未返回")
	}
}
