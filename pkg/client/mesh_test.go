// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMeshServices(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/services" && r.Method == http.MethodGet {
			// I66：断言 token 复用注入链路——mesh 信令复用 auth_token 携带 Bearer
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "SproxySig ") {
				http.Error(w, fmt.Sprintf("missing/mismatched Authorization: %q", got), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"sg-ssh","node":"exit-1","addr":"sg.example.com:22"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	svcs, err := c.MeshServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0].Name != "sg-ssh" || svcs[0].Node != "exit-1" {
		t.Fatalf("unexpected services: %+v", svcs)
	}
}

func TestMeshConnect_NotFound(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/services" {
			// I66：服务发现同样复用 auth_token
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "SproxySig ") {
				http.Error(w, fmt.Sprintf("missing/mismatched Authorization: %q", got), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	_, _, err := c.MeshConnect(context.Background(), "missing-svc")
	if err == nil {
		t.Fatal("expected error for missing service")
	}
	if !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMeshConnect_Echo(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// 单个原始 TCP mock 同时服务两个端点：
	//   GET  /api/hub/services  → JSON 服务发现
	//   POST /api/relay/stream  → CONNECT 风格：读请求体 → 写 200 → echo 后续字节
	hub := &mockMeshHub{
		CheckAuth:    true,
		ServicesBody: `[{"name":"echo","node":"leaf","addr":"127.0.0.1:7777"}]`,
		RelayKind:    RelayEcho,
	}
	addr := startMockMeshHub(t, hub)

	c := NewFileClient("http://"+addr, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, node, err := c.MeshConnect(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if node != "leaf" {
		t.Fatalf("unexpected node: %q", node)
	}
	assertMeshEchoData(t, conn, []byte("mesh-echo-test"))
}

// TestMeshConnect_MultiCandidateFallback 验证 MeshConnect 遍历同名服务候选：
// 首个节点地址不可达时尝试下一个，直到成功。
func TestMeshConnect_MultiCandidateFallback(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// 只服务 /api/hub/services：返回两个候选，node-A 用不可达地址，node-B 可达
	// 可达的 echo 后端（纯数据 echo，不做协议解析——hub 已处理 CONNECT）
	reachableAddr := startEchoBackend(t)

	// hub 服务器：services 返回两个候选，relay/stream 走 reachable
	hub := &mockMeshHub{
		CheckAuth:     true,
		ServicesBody:  fmt.Sprintf(`[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"%s"}]`, reachableAddr),
		RelayKind:     RelayProxy,
		ReachableAddr: reachableAddr,
		FailAddr:      "127.0.0.1:1",
		RelayStatus:   "HTTP/1.1 502 Bad Gateway\r\n\r\n",
		Allow:         true,
	}
	hubAddr := startMockMeshHub(t, hub)

	c := NewFileClient("http://"+hubAddr, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// node-A 地址 127.0.0.1:1 不可达，MeshConnect 应回退到 node-B
	conn, node, err := c.MeshConnect(ctx, "svc")
	if err != nil {
		t.Fatalf("MeshConnect should fallback to reachable candidate: %v", err)
	}
	defer conn.Close()
	if node != "node-B" {
		t.Fatalf("expected fallback to node-B, got %q", node)
	}
	// 验证数据面通
	assertMeshEchoData(t, conn, []byte("multi-candidate"))
}

// TestRelayStream_Success_Echo 直接单测 RelayStream：200 建立后数据面 echo 可用（S50）。
func TestRelayStream_Success_Echo(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	hub := &mockMeshHub{RelayKind: RelayEcho}
	addr := startMockMeshHub(t, hub)

	c := NewFileClient("http://" + addr)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := c.RelayStream(ctx, "leaf", "127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertMeshEchoData(t, conn, []byte("relay-echo"))
}

// TestRelayStream_ErrorStatus 验证非 200 状态（502/401/404）返回 error（S50）。
func TestRelayStream_ErrorStatus(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	for _, tc := range []struct {
		name       string
		statusLine string
		wantSubstr string
	}{
		{"bad_gateway", "HTTP/1.1 502 Bad Gateway\x0d\x0a\x0d\x0a", "502"},
		{"unauthorized", "HTTP/1.1 401 Unauthorized\x0d\x0a\x0d\x0a", "401"},
		{"not_found", "HTTP/1.1 404 Not Found\x0d\x0a\x0d\x0a", "404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := &mockMeshHub{RelayKind: RelayStatusFail, RelayStatus: tc.statusLine}
			addr := startMockMeshHub(t, hub)

			c := NewFileClient("http://" + addr)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, err := c.RelayStream(ctx, "leaf", "127.0.0.1:7777")
			if err == nil {
				conn.Close()
				t.Fatalf("expected error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("expected error to contain %q, got %v", tc.wantSubstr, err)
			}
		})
	}
}

// TestRelayStream_HandshakeHang 验证 I33：mock 接受连接但不响应（模拟 hub 半开/黑洞），
// 短 ctx deadline 下握手应在毫秒级超时返回，而非无限阻塞。
func TestRelayStream_HandshakeHang(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
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
				defer c.Close()
				// 读请求但永不写响应，保持连接打开模拟半开
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	c := NewFileClient("http://" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.RelayStream(ctx, "leaf", "127.0.0.1:7777")
	if err == nil {
		t.Fatal("expected error for hung handshake")
	}
	// 握手 deadline = min(ctx 500ms, 30s) = 500ms；-race 下留余量
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handshake hang should resolve quickly, took %v", elapsed)
	}
}

// TestMeshConnect_504Fallback 验证 B4 语义：hub 等待叶子拨号结果超时回 504，
// MeshConnect 应回退到下一候选（I35）。
func TestMeshConnect_504Fallback(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	// 可达 echo 后端
	reachableAddr := startEchoBackend(t)

	hub := &mockMeshHub{
		CheckAuth:     true,
		ServicesBody:  fmt.Sprintf(`[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"%s"}]`, reachableAddr),
		RelayKind:     RelayProxy,
		ReachableAddr: reachableAddr,
		FailAddr:      "127.0.0.1:1",
		RelayStatus:   "HTTP/1.1 504 Gateway Timeout\x0d\x0a\x0d\x0a",
	}
	hubAddr := startMockMeshHub(t, hub)

	c := NewFileClient("http://"+hubAddr, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, node, err := c.MeshConnect(ctx, "svc")
	if err != nil {
		t.Fatalf("MeshConnect should fallback after 504: %v", err)
	}
	defer conn.Close()
	if node != "node-B" {
		t.Fatalf("expected fallback to node-B after 504, got %q", node)
	}
	// 数据面验证
	assertMeshEchoData(t, conn, []byte("after-504"))
}

// TestMeshConnect_AllCandidatesFail 验证所有候选均失败时返回聚合错误（I35）。
func TestMeshConnect_AllCandidatesFail(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	hub := &mockMeshHub{
		CheckAuth:    true,
		ServicesBody: `[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"127.0.0.1:2"}]`,
		RelayKind:    RelayStatusFail,
		RelayStatus:  "HTTP/1.1 502 Bad Gateway\x0d\x0a\x0d\x0a",
	}
	hubAddr := startMockMeshHub(t, hub)

	c := NewFileClient("http://"+hubAddr, WithAccessKey("test-ak", "test-sk"), WithAccessKeyID("skey-aaaaaaaaaaaa"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, _, err := c.MeshConnect(ctx, "svc")
	if err == nil {
		t.Fatal("expected error when all candidates fail")
	}
	if !strings.Contains(err.Error(), "所有候选") {
		t.Fatalf("expected all-candidates error, got %v", err)
	}
}

// TestBufferedNetConn_CloseWrite 验证 CloseWrite 透传到底层 TCPConn（S46）：半关闭后
// 对端 Read 应收到 EOF。
func TestBufferedNetConn_CloseWrite(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverCh := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			serverErr <- aerr
			return
		}
		defer conn.Close()
		buf := make([]byte, 16)
		if _, rerr := conn.Read(buf); rerr != nil {
			serverErr <- rerr
			return
		}
		// 第二次读：对端 CloseWrite（未 Close）后应返回 EOF
		_, rerr := conn.Read(buf)
		close(serverCh)
		serverErr <- rerr
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	bc := &bufferedNetConn{Conn: clientConn, reader: bufio.NewReader(clientConn)}
	if _, err := bc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := bc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	<-serverCh
	if err := <-serverErr; err != io.EOF {
		t.Fatalf("expected EOF on server after CloseWrite, got %v", err)
	}
}

// mockMeshHub 是模拟 mesh hub（服务发现 + CONNECT 中继）的可复用数据驱动构建物。
// 各字段决定服务端行为，测试只需填写结构化配置，不写路由闭包。
type mockMeshHub struct {
	// CheckAuth 为 true 时校验请求 Authorization 是否为 SproxySig（I66）。
	CheckAuth bool
	// ServicesBody 是 GET /api/hub/services 的 JSON 响应体；空则回 404。
	ServicesBody string
	// RelayKind 决定 POST /api/relay/stream 的处理方式。
	RelayKind mockRelayKind
	// RelayStatus 是 RelayStatusFail/RelayProxy 时固定的错误状态行。
	RelayStatus string
	// ReachableAddr 是 RelayProxy 时代理到的可达后端地址。
	ReachableAddr string
	// FailAddr 是 RelayProxy 时命中即返回 RelayStatus 的目标地址。
	FailAddr string
	// Allow 为 RelayProxy 时启用 S97 target/type 校验。
	Allow bool
}

// mockRelayKind 标识 mock hub 对中继请求的处理方式。
type mockRelayKind int

const (
	// RelayEcho 丢弃请求体后回 200 并后续字节原样 echo。
	RelayEcho mockRelayKind = iota
	// RelayStatusFail 恒写 RelayStatus 错误状态行。
	RelayStatusFail
	// RelayProxy 读取请求体并按 addr 决策：要么回 RelayStatus，要么代理到 ReachableAddr。
	RelayProxy
)

// startMockMeshHub 启动一个模拟 hub TCP 服务端，按 cfg 配置分派请求。返回监听地址。
func startMockMeshHub(t *testing.T, cfg *mockMeshHub) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go startMockHubAcceptor(ln, cfg)
	return ln.Addr().String()
}

// startEchoBackend 启动一个可达的纯 echo 后端，返回其监听地址。
func startEchoBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go startEchoAcceptor(ln)
	return ln.Addr().String()
}

// startMockHubAcceptor 接受连接并为每个连接启动一个处理 goroutine。
func startMockHubAcceptor(ln net.Listener, cfg *mockMeshHub) {
	for {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		go serveMockHubConn(conn, cfg)
	}
}

// startEchoAcceptor 接受 echo 后端连接并把每个连接原样回显。
func startEchoAcceptor(ln net.Listener) {
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
}

// serveMockHubConn 读取请求头、校验鉴权，并按状态行分派到 services 或 relay。
func serveMockHubConn(c net.Conn, cfg *mockMeshHub) {
	defer c.Close()
	br := bufio.NewReader(c)
	statusLine, _ := br.ReadString('\n')
	if statusLine == "" {
		return
	}
	headers, contentLength := readMockHubHeaders(br)
	if cfg.CheckAuth && !strings.HasPrefix(headers["authorization"], "SproxySig ") {
		_, _ = io.WriteString(c, "HTTP/1.1 401 Unauthorized\x0d\x0a\x0d\x0a")
		return
	}
	switch {
	case strings.Contains(statusLine, "GET /api/hub/services "):
		if cfg.ServicesBody != "" {
			writeServicesResponse(c, cfg.ServicesBody)
			return
		}
	case strings.Contains(statusLine, "POST /api/relay/stream "):
		serveMockRelay(c, cfg, contentLength, br)
		return
	}
	_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\x0d\x0a\x0d\x0a")
}

// readMockHubHeaders 读取 CONNECT 风格请求头直到空行，返回 headers（小写 key）与 Content-Length。
func readMockHubHeaders(br *bufio.Reader) (map[string]string, int64) {
	headers := map[string]string{}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			break
		}
		if line == "\r\n" || line == "\n" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	contentLength, _ := strconv.ParseInt(headers["content-length"], 10, 64)
	return headers, contentLength
}

// serveMockRelay 按 cfg.RelayKind 处理一个中继请求。
func serveMockRelay(c net.Conn, cfg *mockMeshHub, contentLength int64, br *bufio.Reader) {
	switch cfg.RelayKind {
	case RelayEcho:
		if contentLength > 0 {
			_, _ = io.CopyN(io.Discard, br, contentLength)
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\x0d\x0a\x0d\x0a")
		_, _ = io.Copy(c, br)
		return
	case RelayProxy:
		serveMockRelayProxy(c, cfg, br, contentLength)
		return
	default:
		// 先读尽请求体再回状态行：Windows 下若直接 Close 且残留未读数据，
		// TCP RST 会把已缓冲的响应一并丢弃，导致客户端「读响应状态失败」。
		if contentLength > 0 {
			_, _ = io.CopyN(io.Discard, br, contentLength)
		}
		_, _ = io.WriteString(c, cfg.RelayStatus)
	}
}

// serveMockRelayProxy 解析中继请求体按目标地址决策：命中失败地址回错、否则代理到后端。
func serveMockRelayProxy(c net.Conn, cfg *mockMeshHub, br *bufio.Reader, contentLength int64) {
	body := make([]byte, contentLength)
	_, _ = io.ReadFull(br, body)
	var req struct {
		Target string `json:"target"`
		Type   string `json:"type"`
		Addr   string `json:"addr"`
	}
	_ = json.Unmarshal(body, &req)
	if cfg.Allow && (req.Type != "tcp" || req.Target == "") {
		_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\x0d\x0a\x0d\x0a")
		return
	}
	if req.Addr == cfg.FailAddr {
		_, _ = io.WriteString(c, cfg.RelayStatus)
		return
	}
	up, uerr := net.Dial("tcp", cfg.ReachableAddr)
	if uerr != nil {
		return
	}
	defer up.Close()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\x0d\x0a\x0d\x0a")
	writeRelayProxy(c, br, up)
}

// writeRelayProxy 把 br→conn、conn→up 双向代理直到一端关闭。
func writeRelayProxy(conn net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}

// writeServicesResponse 写一个 services 发现响应（JSON 服务列表）。
func writeServicesResponse(conn net.Conn, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\x0d\x0aContent-Type: application/json\x0d\x0aContent-Length: %d\x0d\x0a\x0d\x0a%s", len(body), body)
}

// assertMeshEchoData 验证 mesh 数据面回显往返一致。
func assertMeshEchoData(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}
}
