// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// runClient 是协议级运行入口：构造 Server（输出到 buf），喂输入，返回输出行。
func runClient(t *testing.T, srv *Server, inputs ...string) []string {
	t.Helper()
	var in strings.Builder
	for _, s := range inputs {
		in.WriteString(s)
		in.WriteString("\n")
	}
	var out bytes.Buffer
	srv2 := NewServer(strings.NewReader(in.String()), &out, srv.tools)
	_ = srv2.Serve(context.Background())
	return strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
}

// jsonLine 编码单行 JSON-RPC 消息。
func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// decodeResp 解码单行 JSON-RPC 响应。
func decodeResp(t *testing.T, line string) response {
	t.Helper()
	var r response
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("解析响应 %q: %v", line, err)
	}
	return r
}

// mockServer 是 sproxy 兼容的 HTTP mock（127.0.0.1 loopback），覆盖
// read_file/write_file/delete 用到的 /download /upload /delete 端点。
// files 预置文件名→内容；uploads 收集上传。
func mockServer(t *testing.T, files map[string]string) (*httptest.Server, *map[string]string) {
	t.Helper()
	uploads := &map[string]string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			name := r.URL.Query().Get("filename")
			content, ok := files[name]
			if !ok {
				http.Error(w, `{"success":false,"message":"file not found"}`, http.StatusNotFound)
				return
			}
			sum := sha256.Sum256([]byte(content))
			w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
			w.Header().Set("X-File-MTime", "0")
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
		case "/upload":
			if r.Header.Get("X-File-Checksum") == "" {
				http.Error(w, `{"success":false,"message":"missing checksum"}`, http.StatusBadRequest)
				return
			}
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer f.Close()
			data, err := io.ReadAll(f)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			name := r.Header.Get("X-File-Path")
			(*uploads)[name] = string(data)
			files[name] = string(data)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"message":"ok","file_checksum":"` + sha256HexBytes(data) + `"}`))
		case "/delete":
			name := r.URL.Query().Get("filename")
			wantCS := r.Header.Get("X-File-Checksum")
			content, ok := files[name]
			if !ok {
				http.Error(w, `{"success":false,"message":"not found"}`, http.StatusNotFound)
				return
			}
			if sha256HexBytes([]byte(content)) != wantCS {
				http.Error(w, `{"success":false,"message":"checksum mismatch"}`, http.StatusBadRequest)
				return
			}
			delete(files, name)
			_, _ = w.Write([]byte(`{"success":true,"message":"deleted"}`))
		case "/api/files/stat":
			name := r.URL.Query().Get("filename")
			content, ok := files[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("X-File-Size", fmt.Sprintf("%d", len(content)))
			w.Header().Set("X-File-Checksum", sha256HexBytes([]byte(content)))
			w.Header().Set("X-File-MTime", "0")
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, uploads
}

func sha256HexBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newFileClientFor 构造连到 mock 的 FileClient（SproxySig 签名走 WithAccessKey——
// mock 不验签，仅验证管线装配不报错）。
func newFileClientFor(t *testing.T, ts *httptest.Server) *client.FileClient {
	t.Helper()
	c := client.NewFileClient(ts.URL, client.WithAccessKey("ak-test", "sk-test"), client.WithAccessKeyID("sk-1"))
	return c
}

// ---------------------------------------------------------------------------
// 1. initialize 握手
// ---------------------------------------------------------------------------

// TestServer_InitializeHandshake 验证 initialize：协议版本协商 + capabilities.tools。
func TestServer_InitializeHandshake(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
	)
	if len(lines) != 1 {
		t.Fatalf("期望 1 条响应，got %d: %v", len(lines), lines)
	}
	r := decodeResp(t, lines[0])
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
		t.Fatalf("capabilities 类型 = %T, want map", result["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities 缺少 tools")
	}
}

// TestServer_Initialize_UnsupportedProtocolVersion 验证不支持协议版本 → -32602。
func TestServer_Initialize_UnsupportedProtocolVersion(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2099-01-01"}}),
	)
	r := decodeResp(t, lines[0])
	if r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatalf("期望 -32602，got %+v", r.Error)
	}
}

// ---------------------------------------------------------------------------
// 2. tools/list 返回完整工具清单
// ---------------------------------------------------------------------------

// TestServer_ToolsList_AfterInitialize 验证初始化后 tools/list 返回完整工具清单。
func TestServer_ToolsList_AfterInitialize(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{})
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}),
	)
	if len(lines) != 2 {
		t.Fatalf("期望 2 条响应，got %d", len(lines))
	}
	r := decodeResp(t, lines[1])
	if r.Error != nil {
		t.Fatalf("tools/list 返回错误: %+v", r.Error)
	}
	result, ok := r.Result.(map[string]any)
	if !ok {
		t.Fatalf("result 类型 = %T", r.Result)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools 类型 = %T", result["tools"])
	}
	names := map[string]bool{}
	for _, item := range tools {
		def := item.(map[string]any)
		name := def["name"].(string)
		names[name] = true
		if _, ok := def["inputSchema"]; !ok {
			t.Errorf("工具 %s 缺少 inputSchema", name)
		}
	}
	for _, want := range []string{toolReadFile, toolWriteFile, toolListFiles, toolSearch, toolStat, toolMkdir, toolDelete, toolShareCreate, toolCloudDownload} {
		if !names[want] {
			t.Errorf("工具清单缺少 %s（共 %d 个）", want, len(tools))
		}
	}
}

// TestServer_ToolsList_EmptyRegistry 验证无工具注册表（nil）时 tools/list 为空数组。
func TestServer_ToolsList_EmptyRegistry(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}),
	)
	r := decodeResp(t, lines[1])
	result := r.Result.(map[string]any)
	if tools, ok := result["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("空注册表应返回空 tools，got %#v", result["tools"])
	}
}

// ---------------------------------------------------------------------------
// 3. tools/call read_file 往返
// ---------------------------------------------------------------------------

// TestServer_ToolsCall_ReadFile 验证 read_file 往返：httptest mock 模拟 GET /download，
// 返回文件文本内容。
func TestServer_ToolsCall_ReadFile(t *testing.T) {
	t.Parallel()

	files := map[string]string{"dir/a.txt": "hello mcp"}
	ts, _ := mockServer(t, files)
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolReadFile, "arguments": map[string]any{"filename": "dir/a.txt"}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error != nil {
		t.Fatalf("read_file 返回错误: %+v", r.Error)
	}
	result := r.Result.(map[string]any)
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 长度 = %d, want 1", len(content))
	}
	text := content[0].(map[string]any)["text"].(string)
	if text != "hello mcp" {
		t.Errorf("read_file 内容 = %q, want hello mcp", text)
	}
}

// TestServer_ToolsCall_ReadFile_NotFound 验证 read_file 404 → -32603（错误路径不当成功）。
func TestServer_ToolsCall_ReadFile_NotFound(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{})
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolReadFile, "arguments": map[string]any{"filename": "missing.txt"}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error == nil {
		t.Fatal("read_file 404 应返回错误，got 成功")
	}
	if r.Error.Code != CodeInternalError {
		t.Errorf("错误码 = %d, want -32603", r.Error.Code)
	}
	data, ok := r.Error.Data.(map[string]any)
	if !ok {
		t.Fatalf("error.data 类型 = %T, want map", r.Error.Data)
	}
	if msg, _ := data["message"].(string); !strings.Contains(msg, "stat 失败") && !strings.Contains(msg, "下载失败") {
		t.Errorf("data.message = %q，应含失败详情", msg)
	}
}

// ---------------------------------------------------------------------------
// 4. 参数校验：write_file 缺 data / delete 缺 checksum → -32602
// ---------------------------------------------------------------------------

// TestServer_ToolsCall_WriteFile_MissingData 验证 write_file 缺 data → -32602。
func TestServer_ToolsCall_WriteFile_MissingData(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{})
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolWriteFile, "arguments": map[string]any{"filename": "a.txt"}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatalf("write_file 缺 data 应返回 -32602，got %+v", r.Error)
	}
}

// TestServer_ToolsCall_Delete_MissingChecksum 验证 delete 缺 checksum → -32602。
func TestServer_ToolsCall_Delete_MissingChecksum(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{})
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolDelete, "arguments": map[string]any{"filename": "a.txt"}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatalf("delete 缺 checksum 应返回 -32602，got %+v", r.Error)
	}
}

// ---------------------------------------------------------------------------
// 5. 方法未知 / 未初始化调用 tools → 标准错误码
// ---------------------------------------------------------------------------

// TestServer_MethodNotFound 验证未知方法 → -32601。
func TestServer_MethodNotFound(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "bogus/method"}),
	)
	r := decodeResp(t, lines[0])
	if r.Error == nil || r.Error.Code != CodeMethodNotFound {
		t.Fatalf("未知方法应返回 -32601，got %+v", r.Error)
	}
}

// TestServer_ToolsCall_BeforeInitialize 验证未 initialize 即调用 tools/call → 错误。
func TestServer_ToolsCall_BeforeInitialize(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": toolReadFile, "arguments": map[string]any{}}}),
	)
	r := decodeResp(t, lines[0])
	if r.Error == nil {
		t.Fatal("未初始化调用 tools/call 应返回错误")
	}
	if r.Error.Code != CodeInvalidRequest {
		t.Errorf("错误码 = %d, want -32600", r.Error.Code)
	}
}

// TestServer_ToolsList_BeforeInitialize 验证未 initialize 即调用 tools/list → 错误。
func TestServer_ToolsList_BeforeInitialize(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}),
	)
	r := decodeResp(t, lines[0])
	if r.Error == nil || r.Error.Code != CodeInvalidRequest {
		t.Fatalf("未初始化 tools/list 应返回 -32600，got %+v", r.Error)
	}
}

// ---------------------------------------------------------------------------
// 6. notifications/initialized 幂等 / ping / exit
// ---------------------------------------------------------------------------

// TestServer_InitializedNotification_Idempotent 验证 notifications/initialized
// 幂等（无响应、可重复、不破坏会话）。
func TestServer_InitializedNotification_Idempotent(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}),
	)
	// notifications 无响应；仅 initialize + ping 两条响应。
	if len(lines) != 2 {
		t.Fatalf("期望 2 条响应（notifications 不应回包），got %d: %v", len(lines), lines)
	}
	r := decodeResp(t, lines[1])
	if r.Error != nil {
		t.Fatalf("ping 返回错误: %+v", r.Error)
	}
}

// TestServer_Ping 验证 ping → 空 result。
func TestServer_Ping(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}),
	)
	r := decodeResp(t, lines[0])
	if r.Error != nil {
		t.Fatalf("ping 返回错误: %+v", r.Error)
	}
}

// TestServer_Exit 验证 exit 通知 → Serve 返回 ErrShutdown。
func TestServer_Exit(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	srv := NewServer(strings.NewReader(jsonLine(map[string]any{"jsonrpc": "2.0", "method": "exit"})+"\n"), &out, nil)
	err := srv.Serve(context.Background())
	if err != ErrShutdown {
		t.Fatalf("Serve 应返回 ErrShutdown，got %v", err)
	}
}

// TestServer_EOF 验证 EOF → 正常返回 nil。
func TestServer_EOF(t *testing.T) {
	t.Parallel()

	srv := NewServer(strings.NewReader(""), io.Discard, nil)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve EOF 应返回 nil，got %v", err)
	}
}

// TestServer_ParseError_Continues 验证坏帧回 ParseError 后继续读下一帧（不崩进程）。
func TestServer_ParseError_Continues(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	srv := NewServer(strings.NewReader("{not json}\n"+jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})+"\n"), &out, nil)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 条响应（ParseError + ping），got %d: %q", len(lines), out.String())
	}
	r := decodeResp(t, lines[0])
	if r.Error == nil || r.Error.Code != CodeParseError {
		t.Fatalf("坏帧应返回 -32700，got %+v", r.Error)
	}
}

// ---------------------------------------------------------------------------
// write_file / delete 往返
// ---------------------------------------------------------------------------

// TestServer_ToolsCall_WriteFile_RoundTrip 验证 write_file 往返：multipart 上传到 mock。
func TestServer_ToolsCall_WriteFile_RoundTrip(t *testing.T) {
	t.Parallel()

	files := map[string]string{}
	ts, uploads := mockServer(t, files)
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolWriteFile, "arguments": map[string]any{"filename": "b.txt", "data": "write me"}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error != nil {
		t.Fatalf("write_file 返回错误: %+v", r.Error)
	}
	if got := (*uploads)["b.txt"]; got != "write me" {
		t.Errorf("上传内容 = %q, want write me", got)
	}
}

// TestServer_ToolsCall_Delete_RoundTrip 验证 delete 往返：携带 checksum 头删除成功。
func TestServer_ToolsCall_Delete_RoundTrip(t *testing.T) {
	t.Parallel()

	files := map[string]string{"a.txt": "bye"}
	ts, _ := mockServer(t, files)
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	cs := sha256HexBytes([]byte("bye"))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolDelete, "arguments": map[string]any{"filename": "a.txt", "checksum": cs}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error != nil {
		t.Fatalf("delete 返回错误: %+v", r.Error)
	}
	if _, ok := files["a.txt"]; ok {
		t.Error("delete 后文件应被删除")
	}
}

// TestServer_ToolsCall_Delete_ChecksumMismatch 验证 delete checksum 不匹配 → 失败。
func TestServer_ToolsCall_Delete_ChecksumMismatch(t *testing.T) {
	t.Parallel()

	files := map[string]string{"a.txt": "bye"}
	ts, _ := mockServer(t, files)
	srv := NewServer(strings.NewReader(""), io.Discard, NewToolRegistry(newFileClientFor(t, ts), ""))
	lines := runClient(t, srv,
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion}}),
		jsonLine(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": toolDelete, "arguments": map[string]any{"filename": "a.txt", "checksum": strings.Repeat("0", 64)}}}),
	)
	r := decodeResp(t, lines[1])
	if r.Error == nil {
		t.Fatal("checksum 不匹配应返回错误")
	}
	if _, ok := files["a.txt"]; !ok {
		t.Error("checksum 不匹配时文件不应被删除")
	}
}

// ---------------------------------------------------------------------------
// 工具注册表
// ---------------------------------------------------------------------------

// TestToolRegistry_Lookup 验证 Lookup 命中/未命中。
func TestToolRegistry_Lookup(t *testing.T) {
	t.Parallel()

	ts, _ := mockServer(t, map[string]string{})
	reg := NewToolRegistry(newFileClientFor(t, ts), "")
	if _, ok := reg.Lookup(toolStat); !ok {
		t.Error("Lookup(read_file) 应命中")
	}
	if _, ok := reg.Lookup("nonexistent"); ok {
		t.Error("Lookup(nonexistent) 不应命中")
	}
}
