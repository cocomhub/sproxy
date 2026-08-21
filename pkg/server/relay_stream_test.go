// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestRelayStreamHandler_BadJSON 验证非法请求体被拒绝。
func TestRelayStreamHandler_BadJSON(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestRelayStreamHandler_MissingFields 验证缺 target/addr 被拒绝。
func TestRelayStreamHandler_MissingFields(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	for _, body := range []string{
		`{"target":"n","type":"tcp","addr":""}`,
		`{"target":"","type":"tcp","addr":"1.2.3.4:80"}`,
		`{"target":"n","type":"udp","addr":"1.2.3.4:80"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/relay/stream", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%s expected 400, got %d", body, w.Code)
		}
	}
}

// TestRelayStream_EndToEnd_Echo 端到端验证任意 TCP 流中继：
// hub → 叶子(mux 流) 出站 echo server，hub → 调用方返回双向字节流。
//
// 拓扑（全部 in-process，127.0.0.1）：
//
//	caller(原始 TCP CONNECT 风格 ⇄ RelayStreamHandler) ⇄ pipe mux ⇄ leaf(relay.Serve) ⇄ TCP echo
func TestRelayStream_EndToEnd_Echo(t *testing.T) {
	// 起一个 TCP echo server（127.0.0.1 回环）
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
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn) // echo
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	// 建立 leaf mux（RoleListener）与 caller mux（RoleDialer）
	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)

	// leaf 侧：relay.Serve 出口模式（dialAllow=true），拨号到 echo
	// 测试用回环 echo server，因此用宽松拨号策略（允许回环）；生产默认严格（DialAllowed）。
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: func(string) bool { return true }})
	}()

	// caller 侧：注册到 RouteTable 并用 RelayStreamHandler 服务
	rt := hub.NewRouteTable()
	rt.Add("leaf-node", callerMux)
	h := NewRelayStreamHandler(rt, testutil.DiscardLogger())
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 用原始 TCP 拨号 + CONNECT 风格请求，模拟 FileClient.RelayStream 的客户端
	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(RelayStreamRequest{Target: "leaf-node", Type: "tcp", Addr: echoAddr})
	reqLine := fmt.Sprintf("POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", addr, len(body))
	if _, werr := io.WriteString(conn, reqLine); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := conn.Write(body); werr != nil {
		t.Fatal(werr)
	}

	// 读响应状态行 + 头直到空行（CONNECT 建立）
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
	}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatal(rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 现在 conn 是纯双向字节流：写 payload 读回 echo
	payload := []byte("hello-relay-stream")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}
	cancel()
	_ = leafErr
}

// TestRelayStreamDialRequest_Framing 验证 dial 帧格式与 hub.DialRequest 一致。
func TestRelayStreamDialRequest_Framing(t *testing.T) {
	d := hub.DialRequest{Dial: "127.0.0.1:22"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// 长度前缀 + JSON
	frame := make([]byte, 4+len(b))
	frame[0] = byte(len(b) >> 24)
	frame[1] = byte(len(b) >> 16)
	frame[2] = byte(len(b) >> 8)
	frame[3] = byte(len(b))
	copy(frame[4:], b)

	var parsed hub.DialRequest
	if err := json.Unmarshal(frame[4:], &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Dial != "127.0.0.1:22" {
		t.Fatalf("unexpected dial: %q", parsed.Dial)
	}
}
