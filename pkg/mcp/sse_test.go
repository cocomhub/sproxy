// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// ---------------------------------------------------------------------------
// SSE 传输（片 4）：GET /sse 事件流 + POST /messages + Bearer 认证
// ---------------------------------------------------------------------------

// readSSEEvent 从 SSE 流读一条事件（event:/data: 行对，空行结束）。
// 返回 event 名与 data 载荷；超时（5s）判失败。
func readSSEEvent(t *testing.T, br *bufio.Reader) (event, data string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("SSE 事件读超时（未收到完整事件）")
		default:
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 SSE 行: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" {
				return event, data
			}
			continue
		}
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			event = strings.TrimSpace(after)
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			data = strings.TrimSpace(after)
		}
	}
}

// newSSETestServer 构造挂载 SSE handler 的测试服务器（127.0.0.1 loopback）。
func newSSETestServer(t *testing.T, tools *ToolRegistry, opts SSEOptions) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewSSEHandler(tools, opts))
	t.Cleanup(ts.Close)
	return ts
}

// openSSEStream 打开 GET /sse 事件流并断言响应头；返回响应体 reader（测试负责 Close）。
func openSSEStream(t *testing.T, ts *httptest.Server, auth string) *bufio.Reader {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sse status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应 text/event-stream, got %q", ct)
	}
	return bufio.NewReader(resp.Body)
}

// postMessage 向 POST /messages 发送一条 JSON-RPC 消息，返回 HTTP 状态码。
func postMessage(t *testing.T, ts *httptest.Server, sessionID, auth, msg string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/messages?sessionId="+sessionID, strings.NewReader(msg))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST /messages: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestSSE_EndpointEventThenInitialize 验证 GET /sse 首帧 endpoint 事件（下发
// /messages?sessionId=<id>）→ POST /messages 发 initialize → 事件流回 message
// 事件（protocolVersion 协商 + capabilities.tools）。
func TestSSE_EndpointEventThenInitialize(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{})
	br := openSSEStream(t, ts, "")

	// 首帧 endpoint 事件：data 形如 /messages?sessionId=<id>。
	event, data := readSSEEvent(t, br)
	if event != "endpoint" {
		t.Fatalf("首帧 event = %q, want endpoint", event)
	}
	sessionID, ok := strings.CutPrefix(data, "/messages?sessionId=")
	if !ok || sessionID == "" {
		t.Fatalf("endpoint data = %q, 应含 /messages?sessionId=<id>", data)
	}

	// POST initialize → 202 Accepted（响应异步经事件流回）。
	msg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}})
	if status := postMessage(t, ts, sessionID, "", msg); status != http.StatusAccepted {
		t.Fatalf("POST /messages status = %d, want 202", status)
	}

	// 事件流收 message 事件，解码为 JSON-RPC 响应。
	event, data = readSSEEvent(t, br)
	if event != "message" {
		t.Fatalf("响应事件 event = %q, want message", event)
	}
	r := decodeResp(t, data)
	if r.Error != nil {
		t.Fatalf("initialize 返回错误: %+v", r.Error)
	}
	result, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result 类型 = %T, want map", r.Result)
	}
	if got := result["protocolVersion"]; got != ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", got, ProtocolVersion)
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities 类型 = %T", result["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities 缺少 tools")
	}
}

// TestSSE_ToolsCall_ReadFile_RoundTrip 验证 SSE 全链路：initialize + tools/call
// read_file → mock sproxy /download → 事件流回文件内容（工具调用经 FileClient
// 薄封装，与会话状态机同管线）。
func TestSSE_ToolsCall_ReadFile_RoundTrip(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{"dir/a.txt": "hello sse"})
	sseTS := newSSETestServer(t, NewToolRegistry(newFileClientFor(t, ts), ""), SSEOptions{})
	br := openSSEStream(t, sseTS, "")

	_, data := readSSEEvent(t, br)
	sessionID, ok := strings.CutPrefix(data, "/messages?sessionId=")
	if !ok || sessionID == "" {
		t.Fatalf("endpoint data = %q, 应含 sessionId", data)
	}

	initMsg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}})
	if status := postMessage(t, sseTS, sessionID, "", initMsg); status != http.StatusAccepted {
		t.Fatalf("initialize POST status = %d", status)
	}
	if _, data = readSSEEvent(t, br); data == "" {
		t.Fatal("initialize 应回 message 事件")
	}
	callMsg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolReadFile, "arguments": map[string]any{"filename": "dir/a.txt"}}})
	if status := postMessage(t, sseTS, sessionID, "", callMsg); status != http.StatusAccepted {
		t.Fatalf("tools/call POST status = %d", status)
	}
	event, data := readSSEEvent(t, br)
	if event != "message" {
		t.Fatalf("响应事件 event = %q, want message", event)
	}
	r := decodeResp(t, data)
	if r.Error != nil {
		t.Fatalf("read_file 返回错误: %+v", r.Error)
	}
	result := r.Result.(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if text != "hello sse" {
		t.Errorf("read_file 内容 = %q, want hello sse", text)
	}
}

// TestSSE_PostUnknownSession_404 验证 POST /messages 携带未知 sessionId → 404
// （会话不存在，事件流之外的非法端点不产生幽灵会话）。
func TestSSE_PostUnknownSession_404(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{})
	msg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if status := postMessage(t, ts, "no-such-session", "", msg); status != http.StatusNotFound {
		t.Fatalf("未知 session status = %d, want 404", status)
	}
}

// TestSSE_Auth_RequiresBearer 验证 Bearer 门禁（token 非空时）：无/错 token 的
// GET /sse → 401 空 body；正确 token → 200 + 事件流（常量时间比较，不泄露 SK）。
func TestSSE_Auth_RequiresBearer(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{BearerToken: "sk-secret"})

	// 无 Bearer → 401。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /sse（无 token）: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 token status = %d, want 401", resp.StatusCode)
	}

	// 错误 Bearer → 401。
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err = testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /sse（错 token）: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错 token status = %d, want 401", resp.StatusCode)
	}

	// 正确 Bearer → 200 事件流。
	br := openSSEStream(t, ts, "sk-secret")
	event, _ := readSSEEvent(t, br)
	if event != "endpoint" {
		t.Fatalf("认证通过后首帧 event = %q, want endpoint", event)
	}
}

// TestSSE_PostMessages_AuthRequired 验证 POST /messages 同样受 Bearer 门禁
// （无 token → 401；认证面不因端点而异）。
func TestSSE_PostMessages_AuthRequired(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{BearerToken: "sk-secret"})
	msg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if status := postMessage(t, ts, "any-session", "", msg); status != http.StatusUnauthorized {
		t.Fatalf("POST 无 token status = %d, want 401", status)
	}
}

// TestSSE_NoBearerConfigured_Open 验证 token 为空时端点公开（零回归默认，
// 与仓库 notify feed 的「空 = 公开」惯例一致）。
func TestSSE_NoBearerConfigured_Open(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{})
	br := openSSEStream(t, ts, "")
	event, data := readSSEEvent(t, br)
	if event != "endpoint" || !strings.Contains(data, "sessionId=") {
		t.Fatalf("无认证时应直接下发 endpoint 事件, got event=%q data=%q", event, data)
	}
}

// TestSSE_MethodNotAllowed 验证非 GET /sse、非 POST /messages 的方法/路径 → 404。
func TestSSE_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/messages", nil)
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /messages: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /messages status = %d, want 404（仅 POST 收消息）", resp.StatusCode)
	}
}

// TestSSE_PingRoundTrip 验证 SSE 下 ping → message 事件回空 result
// （协议最小往返，避免长握手场景下的状态机回归）。
func TestSSE_PingRoundTrip(t *testing.T) {
	t.Parallel()

	ts := newSSETestServer(t, nil, SSEOptions{})
	br := openSSEStream(t, ts, "")
	_, data := readSSEEvent(t, br)
	sessionID, ok := strings.CutPrefix(data, "/messages?sessionId=")
	if !ok || sessionID == "" {
		t.Fatalf("endpoint data = %q", data)
	}

	msg := jsonLine(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "ping"})
	if status := postMessage(t, ts, sessionID, "", msg); status != http.StatusAccepted {
		t.Fatalf("ping POST status = %d", status)
	}
	event, data := readSSEEvent(t, br)
	if event != "message" {
		t.Fatalf("响应事件 event = %q, want message", event)
	}
	var r response
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		t.Fatalf("ping 响应解析失败: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("ping 返回错误: %+v", r.Error)
	}
	if r.ID == nil || string(r.ID) != "9" {
		t.Fatalf("ping 响应 id = %s, want 9", string(r.ID))
	}
}
