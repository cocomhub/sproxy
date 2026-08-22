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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/services" && r.Method == http.MethodGet {
			// I66：断言 token 复用注入链路——mesh 信令复用 auth_token 携带 Bearer
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
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

	c := NewFileClient(ts.URL, WithAuthToken("test-token"))
	svcs, err := c.MeshServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0].Name != "sg-ssh" || svcs[0].Node != "exit-1" {
		t.Fatalf("unexpected services: %+v", svcs)
	}
}

func TestMeshConnect_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/services" {
			// I66：服务发现同样复用 auth_token
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
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

	c := NewFileClient(ts.URL, WithAuthToken("test-token"))
	_, _, err := c.MeshConnect(context.Background(), "missing-svc")
	if err == nil {
		t.Fatal("expected error for missing service")
	}
	if !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMeshConnect_Echo(t *testing.T) {
	// 单个原始 TCP mock 同时服务两个端点：
	//   GET  /api/hub/services  → JSON 服务发现
	//   POST /api/relay/stream  → CONNECT 风格：读请求体 → 写 200 → echo 后续字节
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
				br := bufio.NewReader(c)
				statusLine, lerr := br.ReadString('\n')
				if lerr != nil {
					return
				}
				// 读请求头，直到空行
				contentLength := int64(0)
				headers := map[string]string{}
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok {
						headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
					}
				}
				contentLength, _ = strconv.ParseInt(headers["content-length"], 10, 64)
				// I66：所有 mesh 信令请求都必须携带复用后的 auth_token Bearer
				if headers["authorization"] != "Bearer test-token" {
					_, _ = io.WriteString(c, "HTTP/1.1 401 Unauthorized\r\n\r\n")
					return
				}

				switch {
				case strings.Contains(statusLine, "GET /api/hub/services "):
					body := `[{"name":"echo","node":"leaf","addr":"127.0.0.1:7777"}]`
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
					return
				case strings.Contains(statusLine, "POST /api/relay/stream "):
					// 读掉请求 body
					if contentLength > 0 {
						_, _ = io.CopyN(io.Discard, br, contentLength)
					}
					// 建立：写 200，然后 echo 后续字节
					_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
					_, _ = io.Copy(c, br)
					return
				default:
					_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\n\r\n")
					return
				}
			}(conn)
		}
	}()

	c := NewFileClient("http://"+ln.Addr().String(), WithAuthToken("test-token"))
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

	payload := []byte("mesh-echo-test")
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

// TestMeshConnect_MultiCandidateFallback 验证 MeshConnect 遍历同名服务候选：
// 首个节点地址不可达时尝试下一个，直到成功。
func TestMeshConnect_MultiCandidateFallback(t *testing.T) {
	// 只服务 /api/hub/services：返回两个候选，node-A 用不可达地址，node-B 可达
	// 可达的 echo 后端（纯数据 echo，不做协议解析——hub 已处理 CONNECT）
	reachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer reachable.Close()
	go func() {
		for {
			conn, aerr := reachable.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c) // 纯 echo
			}(conn)
		}
	}()

	// hub 服务器：services 返回两个候选，relay/stream 走 reachable
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	reachableAddr := reachable.Addr().String()
	go func() {
		for {
			conn, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				statusLine, lerr := br.ReadString('\n')
				if lerr != nil {
					return
				}
				contentLength := int64(0)
				headers := map[string]string{}
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok {
						headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
					}
				}
				contentLength, _ = strconv.ParseInt(headers["content-length"], 10, 64)
				// I66：mesh 信令请求必须携带复用后的 auth_token Bearer
				if headers["authorization"] != "Bearer test-token" {
					_, _ = io.WriteString(c, "HTTP/1.1 401 Unauthorized\r\n\r\n")
					return
				}
				switch {
				case strings.Contains(statusLine, "GET /api/hub/services "):
					body := fmt.Sprintf(`[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"%s"}]`, reachableAddr)
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
					return
				case strings.Contains(statusLine, "POST /api/relay/stream "):
					body := make([]byte, contentLength)
					_, _ = io.ReadFull(br, body)
					var req struct {
						Target string `json:"target"`
						Type   string `json:"type"`
						Addr   string `json:"addr"`
					}
					_ = json.Unmarshal(body, &req)
					// S97：校验 relay 请求体 target/type 字段与宣告的服务一致
					if req.Type != "tcp" || (req.Target != "node-A" && req.Target != "node-B") {
						_, _ = io.WriteString(c, "HTTP/1.1 400 Bad Request\x0d\x0a\x0d\x0a")
						return
					}
					// 按请求体里的 addr 决定是否可达：127.0.0.1:1（node-A）不可达 → 返回不发 200
					if req.Addr == "127.0.0.1:1" {
						_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
						return
					}
					// 转发到 reachable（模拟 hub 代理）
					up, uerr := net.Dial("tcp", reachableAddr)
					if uerr != nil {
						return
					}
					defer up.Close()
					_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
					done := make(chan struct{}, 2)
					go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
					go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
					<-done
					return
				default:
					_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\n\r\n")
					return
				}
			}(conn)
		}
	}()

	c := NewFileClient("http://"+hubLn.Addr().String(), WithAuthToken("test-token"))
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
	payload := []byte("multi-candidate")
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

// TestRelayStream_Success_Echo 直接单测 RelayStream：200 建立后数据面 echo 可用（S50）。
func TestRelayStream_Success_Echo(t *testing.T) {
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
				br := bufio.NewReader(c)
				if _, lerr := br.ReadString('\n'); lerr != nil {
					return
				}
				contentLength := int64(0)
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\x0d\x0a" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				if contentLength > 0 {
					_, _ = io.CopyN(io.Discard, br, contentLength)
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\x0d\x0a\x0d\x0a")
				_, _ = io.Copy(c, br) // echo
			}(conn)
		}
	}()

	c := NewFileClient("http://" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, err := c.RelayStream(ctx, "leaf", "127.0.0.1:7777")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload := []byte("relay-echo")
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

// TestRelayStream_ErrorStatus 验证非 200 状态（502/401/404）返回 error（S50）。
func TestRelayStream_ErrorStatus(t *testing.T) {
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
						br := bufio.NewReader(c)
						if _, lerr := br.ReadString('\n'); lerr != nil {
							return
						}
						contentLength := int64(0)
						for {
							line, rerr := br.ReadString('\n')
							if rerr != nil {
								return
							}
							if line == "\x0d\x0a" || line == "\n" {
								break
							}
							k, v, ok := strings.Cut(line, ":")
							if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
								contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
							}
						}
						if contentLength > 0 {
							_, _ = io.CopyN(io.Discard, br, contentLength)
						}
						_, _ = io.WriteString(c, tc.statusLine)
					}(conn)
				}
			}()

			c := NewFileClient("http://" + ln.Addr().String())
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
	// 可达 echo 后端
	reachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer reachable.Close()
	go func() {
		for {
			conn, aerr := reachable.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	reachableAddr := reachable.Addr().String()
	go func() {
		for {
			conn, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				statusLine, lerr := br.ReadString('\n')
				if lerr != nil {
					return
				}
				contentLength := int64(0)
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\x0d\x0a" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				switch {
				case strings.Contains(statusLine, "GET /api/hub/services "):
					body := fmt.Sprintf(`[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"%s"}]`, reachableAddr)
					fmt.Fprintf(c, "HTTP/1.1 200 OK\x0d\x0aContent-Type: application/json\x0d\x0aContent-Length: %d\x0d\x0a\x0d\x0a%s", len(body), body)
					return
				case strings.Contains(statusLine, "POST /api/relay/stream "):
					body := make([]byte, contentLength)
					_, _ = io.ReadFull(br, body)
					var req struct {
						Addr string `json:"addr"`
					}
					_ = json.Unmarshal(body, &req)
					if req.Addr == "127.0.0.1:1" {
						// 模拟 hub 12s 决策超时后回 504
						_, _ = io.WriteString(c, "HTTP/1.1 504 Gateway Timeout\x0d\x0a\x0d\x0a")
						return
					}
					up, uerr := net.Dial("tcp", reachableAddr)
					if uerr != nil {
						return
					}
					defer up.Close()
					_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\x0d\x0a\x0d\x0a")
					done := make(chan struct{}, 2)
					go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
					go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
					<-done
					return
				default:
					_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\x0d\x0a\x0d\x0a")
					return
				}
			}(conn)
		}
	}()

	c := NewFileClient("http://"+hubLn.Addr().String(), WithAuthToken("test-token"))
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
	payload := []byte("after-504")
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

// TestMeshConnect_AllCandidatesFail 验证所有候选均失败时返回聚合错误（I35）。
func TestMeshConnect_AllCandidatesFail(t *testing.T) {
	hubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hubLn.Close()
	go func() {
		for {
			conn, aerr := hubLn.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				statusLine, lerr := br.ReadString('\n')
				if lerr != nil {
					return
				}
				contentLength := int64(0)
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\x0d\x0a" || line == "\n" {
						break
					}
					k, v, ok := strings.Cut(line, ":")
					if ok && strings.ToLower(strings.TrimSpace(k)) == "content-length" {
						contentLength, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
					}
				}
				switch {
				case strings.Contains(statusLine, "GET /api/hub/services "):
					body := `[{"name":"svc","node":"node-A","addr":"127.0.0.1:1"},{"name":"svc","node":"node-B","addr":"127.0.0.1:2"}]`
					fmt.Fprintf(c, "HTTP/1.1 200 OK\x0d\x0aContent-Type: application/json\x0d\x0aContent-Length: %d\x0d\x0a\x0d\x0a%s", len(body), body)
					return
				case strings.Contains(statusLine, "POST /api/relay/stream "):
					body := make([]byte, contentLength)
					_, _ = io.ReadFull(br, body)
					var req struct {
						Addr string `json:"addr"`
					}
					_ = json.Unmarshal(body, &req)
					_ = req // 两个候选都失败 → 502
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\x0d\x0a\x0d\x0a")
					return
				default:
					_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\x0d\x0a\x0d\x0a")
					return
				}
			}(conn)
		}
	}()

	c := NewFileClient("http://"+hubLn.Addr().String(), WithAuthToken("test-token"))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, _, err = c.MeshConnect(ctx, "svc")
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
