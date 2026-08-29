// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
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

// TestDialAllowed_ResolvedAddress 验证主机名放行时返回解析后的 IP:port（防 rebinding TOCTOU），
// 且 IPv6 返回带方括号的 host:port（I21：之前 ip.String()+":"+port 拼出非法地址）。
func TestDialAllowed_ResolvedAddress(t *testing.T) {
	resolved, ok := DialAllowed("8.8.8.8:53")
	if !ok {
		t.Fatal("expected public IP allowed")
	}
	if resolved != "8.8.8.8:53" {
		t.Fatalf("expected IP:port passthrough, got %q", resolved)
	}

	resolved6, ok := DialAllowed("[2606:4700:4700::1111]:443")
	if !ok {
		t.Fatal("expected public IPv6 allowed")
	}
	if resolved6 != "[2606:4700:4700::1111]:443" {
		t.Fatalf("expected bracketed IPv6, got %q", resolved6)
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

// TestNewDialPolicy_InvalidCIDR_Warns 验证非法 CIDR 不再静默丢弃，而是记录 Warn（S25）。
func TestNewDialPolicy_InvalidCIDR_Warns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_ = NewDialPolicy([]string{"192.168.0.0/16", "not-a-cidr"})
	if !strings.Contains(buf.String(), "忽略非法 CIDR") {
		t.Fatalf("expected invalid CIDR warning, got %q", buf.String())
	}
}

// TestNewServiceDialPolicy 验证出口拨号策略对节点自身宣告的服务地址做精确放行，
// 其余回落既有 NewDialPolicy 逻辑（公网 + 白名单 CIDR）。
func TestNewServiceDialPolicy(t *testing.T) {
	// "127.0.0.1"（无端口）放入宣告列表，确保精确命中后再被 SplitHostPort 拒绝
	// 的分支被真正执行（I59：之前该用例未命中 exact，走的是 base 回落）。
	svcAddrs := []string{"127.0.0.1:10022", "localhost:10022", "10.0.0.5:22", "127.0.0.1"}
	policy := NewServiceDialPolicy(nil, svcAddrs)

	tests := []struct {
		name string
		addr string
		want bool
	}{
		// 精确命中宣告地址（IP / 主机名 / 私网 IP）→ 放行
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
			if !tc.want {
				return
			}
			switch tc.name {
			case "announced-hostname":
				// H1-S2：主机名宣告解析为 IP:port（localhost 解析平台相关，如 ::1 或
				// 127.0.0.1），断言"是 IP 且端口正确"即可，不断言具体 IP。
				host, port, err := net.SplitHostPort(got)
				if err != nil || port != "10022" || net.ParseIP(host) == nil {
					t.Fatalf("policy(%q) resolved = %q, want IP:10022", tc.addr, got)
				}
			default:
				if got != tc.addr {
					t.Fatalf("policy(%q) resolved = %q, want passthrough %q", tc.addr, got, tc.addr)
				}
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
// 读侧用 goroutine + select ctx.Done() 包裹，防止 serveHTTP 未写响应时 io.ReadFull
// 无限阻塞（I61）。
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
	respMeta, rerr := readTunnelResponse(ctx, clientStream)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if respMeta.Status != http.StatusOK {
		t.Fatalf("expected 200, got %d", respMeta.Status)
	}
	if respMeta.Headers.Get("X-Backend") != "relay" {
		t.Fatalf("expected X-Backend header relayed, got %q", respMeta.Headers.Get("X-Backend"))
	}
	// 读 body（固定长度，服务端不关流所以不能 ReadAll 等 EOF）
	body := make([]byte, len("backend-ok"))
	if err := readFullWithTimeout(ctx, clientStream, body, "响应 body"); err != nil {
		t.Fatalf("read body: %v", err)
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

// TestServeHTTP_BackendUnreachable_NoPanic 验证后端不可达时 serveHTTP 返回不 panic，
// 且流随后被关闭（原 TestServeHTTP_BadMeta 名实不符——它测的其实是"后端不可达"，
// 而非坏 metadata 解析，I60）。坏 metadata 解析路径由 TestServe_BadMeta_ClosesStream
// 覆盖。
func TestServeHTTP_BackendUnreachable_NoPanic(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 调用方端开流（serveHTTP 对 GET 不读 body，无需写数据）
	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	serverDone := make(chan error, 1)
	go func() {
		stream, aerr := serverMux.Accept(ctx)
		if aerr != nil {
			serverDone <- aerr
			return
		}
		defer stream.Close()
		httpClient := &http.Client{Timeout: 5 * time.Second}
		serveHTTP(ctx, stream, "http://127.0.0.1:1", tunnel.Request{Method: "GET", URL: "/x"}, httpClient, testLogger())
		serverDone <- nil
	}()

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("serveHTTP 端失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serveHTTP 应返回而未返回")
	}

	// 流应已被关闭（goroutine defer stream.Close()）
	expectStreamClosed(ctx, t, clientStream, "serveHTTP 后端不可达")
}

// TestServe_BadMeta_ClosesStream 验证 Serve 层对非法 metadata 帧的处理：客户端写
// 垃圾帧（非 JSON、非 dial 帧）→ Serve 解析失败 → 关闭该流（读侧返回错误），且
// 不 panic。补齐 I60 的解析路径覆盖与 H1-T1 的流关闭断言。
func TestServe_BadMeta_ClosesStream(t *testing.T) {
	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- Serve(ctx, serverMux, "http://127.0.0.1:1", false, &http.Client{Timeout: 5 * time.Second}, testLogger())
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	// 写入垃圾 metadata（长度 5，内容不是 JSON，也不是 dial 帧）
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 5)
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write([]byte("12345")); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 读侧应很快返回错误（Serve 端解析失败后 defer s.Close() 关闭流）
	expectStreamClosed(ctx, t, clientStream, "坏 metadata 流")
}

// TestServe_DialFrameDispatch 验证 Serve 的 dial 帧分发：客户端写 dial 帧后进入
// 字节中继，数据经出口 TCP 回显（I62）。
func TestServe_DialFrameDispatch(t *testing.T) {
	echoAddr := startEchoServer(t)

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 放行策略：任意地址都允许（本测试只拨本机 echo）
	policy := func(addr string) (string, bool) { return addr, true }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	// 写 dial 帧
	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: echoAddr})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	if _, werr := clientStream.Write(lenBuf); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := clientStream.Write(dialMeta); werr != nil {
		t.Fatal(werr)
	}

	// 写数据 + 半关闭
	payload := []byte("ping-through-tunnel")
	if _, werr := clientStream.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	// 读回显
	got := make([]byte, len(payload))
	if err := readFullWithTimeout(ctx, clientStream, got, "回显数据"); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected echo %q, got %q", payload, got)
	}
}

// TestServe_DialAllowGate 验证 dialAllow=false 时 dial 帧被拒绝：流被关闭且目标
// listener 零 accept（I62 门控）。
func TestServe_DialAllowGate(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", false, &http.Client{Timeout: 5 * time.Second}, testLogger())
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: ln.Addr().String()})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(dialMeta)
	_ = clientStream.CloseWrite()

	expectStreamClosed(ctx, t, clientStream, "dialAllow=false 流")
	if n := accepted.Load(); n != 0 {
		t.Fatalf("expected zero accepts when dialAllow=false, got %d", n)
	}
}

// TestServe_DialPolicyRejected 验证拨号策略拒绝时 dial 帧不拨号、流被关闭（I62）。
func TestServe_DialPolicyRejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 策略一律拒绝
	policy := func(addr string) (string, bool) { return "", false }
	go func() {
		_ = Serve(ctx, serverMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testLogger(), ServeOptions{DialPolicy: policy})
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	dialMeta, _ := json.Marshal(hub.DialRequest{Dial: ln.Addr().String()})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(dialMeta)))
	_, _ = clientStream.Write(lenBuf)
	_, _ = clientStream.Write(dialMeta)
	_ = clientStream.CloseWrite()

	expectStreamClosed(ctx, t, clientStream, "策略拒绝流")
	if n := accepted.Load(); n != 0 {
		t.Fatalf("expected zero accepts when policy rejects, got %d", n)
	}
}

// TestPumpBidirectional 验证 pump 双向泵送：客户端写 payload + CloseWrite 后能读回
// 完整回显（不截断在途响应，C1），且 pump 正常返回。
func TestPumpBidirectional(t *testing.T) {
	echoAddr := startEchoServer(t)
	remote, err := net.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		s, aerr := serverMux.Accept(ctx)
		if aerr != nil {
			serverDone <- aerr
			return
		}
		// 不在 goroutine 内全关流：pump 的 s.CloseWrite() 半关闭已足够让客户端读到
		// 回显，全关会触发 mux 关闭竞态（done 已关但 dataCh 有缓冲数据时 Read 可能
		// 随机丢数据），导致回显被截断（mux bug 另行跟踪）。
		pump(s, remote, 1*time.Second)
		_ = s.Close()
		serverDone <- nil
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	payload := []byte("ping-through-tunnel")
	if _, werr := clientStream.Write(payload); werr != nil {
		t.Fatal(werr)
	}
	_ = clientStream.CloseWrite()

	got := make([]byte, len(payload))
	if err := readFullWithTimeout(ctx, clientStream, got, "回显数据"); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected echo %q, got %q", payload, got)
	}

	// 双向都完成（echo 关闭后 remote EOF），pump 应正常返回
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("pump 侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("pump 未在双向完成后返回")
	}
}

// TestPump_NonCooperativeRemote_ForceClose 验证非合作远端（对 FIN 不回应、不关闭）
// 在宽限期结束后被强制关闭两端，pump 返回、流被关（C1 防泄漏）。
func TestPump_NonCooperativeRemote_ForceClose(t *testing.T) {
	// 非合作服务：accept 后既不读也不写也不关（挂起）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				time.Sleep(2 * time.Second)
				_ = c.Close()
			}(conn)
		}
	}()

	remote, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	pipeA, pipeB := xfertest.Pipe()
	serverMux := mux.New(pipeA, mux.RoleListener)
	clientMux := mux.New(pipeB, mux.RoleDialer)
	defer serverMux.Close()
	defer clientMux.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	pumpDone := make(chan struct{})
	go func() {
		s, aerr := serverMux.Accept(ctx)
		if aerr != nil {
			close(pumpDone)
			return
		}
		defer s.Close()
		// 宽限期 200ms：远小于测试 ctx 5s，验证非合作远端被强制关闭
		pump(s, remote, 200*time.Millisecond)
		close(pumpDone)
	}()

	clientStream, oerr := clientMux.Open(ctx)
	if oerr != nil {
		t.Fatal(oerr)
	}
	defer clientStream.Close()

	_, _ = clientStream.Write([]byte("hello"))
	_ = clientStream.CloseWrite()

	// pump 应在宽限期后返回（强制关闭两端解除阻塞）
	select {
	case <-pumpDone:
	case <-ctx.Done():
		t.Fatal("pump 未强制关闭非合作远端")
	}

	// 流应已被 pump 关闭
	expectStreamClosed(ctx, t, clientStream, "非合作远端泵送流")
}

// TestServeHTTP_BodyMethods 验证带 body 的方法（POST/PUT/DELETE/OPTIONS）把流作为
// 请求体转发到后端（I62 / S28）。
func TestServeHTTP_BodyMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			var gotBody string
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			}))
			defer backend.Close()

			pipeA, pipeB := xfertest.Pipe()
			serverMux := mux.New(pipeA, mux.RoleListener)
			clientMux := mux.New(pipeB, mux.RoleDialer)
			defer serverMux.Close()
			defer clientMux.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			// 注意：不在 goroutine 内关闭服务端流——mux 关闭竞态（Read 的 select 在
			// dataCh 有缓冲数据但 done 已关时会随机丢数据）会把已写入的响应截断，
			// 测试在读完响应后再关闭流，规避该竞态（mux bug 另行跟踪）。
			serverDone := make(chan error, 1)
			streamCh := make(chan mux.Stream, 1)
			go func() {
				stream, aerr := serverMux.Accept(ctx)
				if aerr != nil {
					serverDone <- aerr
					return
				}
				streamCh <- stream
				serveHTTP(ctx, stream, backend.URL, tunnel.Request{Method: method, URL: "/submit"}, &http.Client{Timeout: 5 * time.Second}, testLogger())
				serverDone <- nil
			}()

			clientStream, oerr := clientMux.Open(ctx)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer clientStream.Close()

			// 直接调 serveHTTP 时流已定位在 metadata 之后（metadata 由 Serve 预先读取），
			// 因此这里只写 body 字节 + CloseWrite，serveHTTP 的 body reader 直接消费。
			if _, werr := clientStream.Write([]byte("hello-body")); werr != nil {
				t.Fatal(werr)
			}
			_ = clientStream.CloseWrite()

			respMeta, rerr := readTunnelResponse(ctx, clientStream)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if respMeta.Status != http.StatusOK {
				t.Fatalf("expected 200, got %d", respMeta.Status)
			}
			if gotBody != "hello-body" {
				t.Fatalf("expected backend body hello-body, got %q", gotBody)
			}
			body := make([]byte, len("ok"))
			if err := readFullWithTimeout(ctx, clientStream, body, "响应 body"); err != nil {
				t.Fatalf("read body: %v", err)
			}
			if string(body) != "ok" {
				t.Fatalf("expected body ok, got %q", body)
			}
			select {
			case s := <-streamCh:
				_ = s.Close()
			default:
			}
		})
	}
}

// TestServeHTTP_URLValidation 验证 I20 的 SSRF 防护：只放行相对路径，绝对 URL /
// host 注入 / userinfo 注入 / 非法编码一律拒绝（返回 400），query 内 @ 正常保留。
func TestServeHTTP_URLValidation(t *testing.T) {
	var gotPath, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	cases := []struct {
		name       string
		url        string
		wantStatus int
		wantPath   string
		wantQuery  string
	}{
		{"relative-ok", "/relayed", http.StatusOK, "/relayed", ""},
		{"relative-query-with-at", "/search?q=user@example.com", http.StatusOK, "/search", "q=user@example.com"},
		{"absolute-rejected", "http://evil.com/x", http.StatusBadRequest, "", ""},
		{"host-injection-rejected", "//evil.com:443/x", http.StatusBadRequest, "", ""},
		{"userinfo-at-rejected", "@evil.com:443/x", http.StatusBadRequest, "", ""},
		{"bad-encoding-rejected", "/x%zz", http.StatusBadRequest, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotQuery = "", ""
			pipeA, pipeB := xfertest.Pipe()
			serverMux := mux.New(pipeA, mux.RoleListener)
			clientMux := mux.New(pipeB, mux.RoleDialer)
			defer serverMux.Close()
			defer clientMux.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			// 不在 goroutine 内关闭服务端流，读完响应后再关（规避 mux 关闭竞态截断响应）。
			serverDone := make(chan error, 1)
			streamCh := make(chan mux.Stream, 1)
			go func() {
				stream, aerr := serverMux.Accept(ctx)
				if aerr != nil {
					serverDone <- aerr
					return
				}
				streamCh <- stream
				serveHTTP(ctx, stream, backend.URL, tunnel.Request{Method: http.MethodGet, URL: tc.url}, &http.Client{Timeout: 5 * time.Second}, testLogger())
				serverDone <- nil
			}()

			clientStream, oerr := clientMux.Open(ctx)
			if oerr != nil {
				t.Fatal(oerr)
			}
			defer clientStream.Close()

			reqMeta, _ := json.Marshal(tunnel.Request{Method: http.MethodGet, URL: tc.url})
			lenBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lenBuf, uint32(len(reqMeta)))
			if _, werr := clientStream.Write(lenBuf); werr != nil {
				t.Fatal(werr)
			}
			if _, werr := clientStream.Write(reqMeta); werr != nil {
				t.Fatal(werr)
			}
			_ = clientStream.CloseWrite()

			respMeta, rerr := readTunnelResponse(ctx, clientStream)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if respMeta.Status != tc.wantStatus {
				t.Fatalf("case %s: expected status %d, got %d", tc.name, respMeta.Status, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if gotPath != tc.wantPath || gotQuery != tc.wantQuery {
					t.Fatalf("case %s: backend got path=%q query=%q, want path=%q query=%q", tc.name, gotPath, gotQuery, tc.wantPath, tc.wantQuery)
				}
			} else if gotPath != "" {
				// 拒绝路径不应触达后端（防 SSRF 数据外泄）
				t.Fatalf("case %s: backend was hit with path=%q, want no forward", tc.name, gotPath)
			}
			select {
			case s := <-streamCh:
				_ = s.Close()
			default:
			}
		})
	}
}

// startEchoServer 启动一个 127.0.0.1 TCP echo 服务（每个连接读多少回写多少，
// 读到 FIN 后关闭），返回监听地址。测试结束自动关闭。
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// readFullWithTimeout 在 goroutine 中执行 io.ReadFull；超时则关闭流解除读阻塞并返回
// 错误。mux.Stream 无 deadline API，故用关闭流解除阻塞（-race 下 goroutine 短暂残留
// 由测试末尾的 mux.Close() 收口）。
func readFullWithTimeout(ctx context.Context, s mux.Stream, buf []byte, what string) error {
	readCh := make(chan error, 1)
	go func() {
		_, rerr := io.ReadFull(s, buf)
		readCh <- rerr
	}()
	select {
	case rerr := <-readCh:
		return rerr
	case <-ctx.Done():
		_ = s.Close()
		return fmt.Errorf("读取%s超时（对端未写入数据）", what)
	}
}

// readTunnelResponse 从流读取 [4B len][tunnel.Response JSON] 响应元数据。
func readTunnelResponse(ctx context.Context, s mux.Stream) (tunnel.Response, error) {
	metaLenBuf := make([]byte, 4)
	if err := readFullWithTimeout(ctx, s, metaLenBuf, "响应元数据长度"); err != nil {
		return tunnel.Response{}, err
	}
	metaLen := binary.BigEndian.Uint32(metaLenBuf)
	metaRaw := make([]byte, metaLen)
	if err := readFullWithTimeout(ctx, s, metaRaw, "响应元数据"); err != nil {
		return tunnel.Response{}, err
	}
	var respMeta tunnel.Response
	if err := json.Unmarshal(metaRaw, &respMeta); err != nil {
		return tunnel.Response{}, err
	}
	return respMeta, nil
}

// expectStreamClosed 断言流被关闭（读返回错误）。用 goroutine + select ctx 包裹防挂死。
func expectStreamClosed(ctx context.Context, t *testing.T, s mux.Stream, what string) {
	t.Helper()
	readCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, rerr := s.Read(buf)
		readCh <- rerr
	}()
	select {
	case rerr := <-readCh:
		if rerr == nil {
			t.Fatalf("%s 应被关闭但读到数据", what)
		}
	case <-ctx.Done():
		t.Fatalf("%s 未被关闭（超时）", what)
	}
}
