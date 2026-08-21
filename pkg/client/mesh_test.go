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
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"sg-ssh","node":"exit-1","addr":"sg.example.com:22"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
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
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
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

	c := NewFileClient("http://" + ln.Addr().String())
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
	svcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svcLn.Close()

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
				for {
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
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
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
					return
				case strings.Contains(statusLine, "POST /api/relay/stream "):
					body := make([]byte, contentLength)
					_, _ = io.ReadFull(br, body)
					var req struct {
						Addr string `json:"addr"`
					}
					_ = json.Unmarshal(body, &req)
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

	c := NewFileClient("http://" + hubLn.Addr().String())
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
