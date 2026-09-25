// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp 实现 Model Context Protocol（MCP）服务器：手写 JSON-RPC 2.0
// 协议层 + 会话状态机 + 工具分派（FileClient 薄封装），供 AI CLI
// （Claude Code / Codex 等）通过 stdio / SSE 传输暴露 sproxy 文件能力。
//
// 设计约束（见 docs/designs/2026-09-24-mcp-server.md 片 4）：
//   - 不引入第三方 MCP SDK，纯标准库实现；
//   - stdio 传输：单行 JSON + '\n' 分隔（Claude/Codex 兼容）；
//   - SSE 传输（远程 HTTP）：GET /sse 事件流（endpoint 下发）+ POST /messages
//     收 JSON-RPC 请求 → 同分派管线 → 事件流回响应；Bearer 认证复用凭据 SK
//     （BearerToken 非空时强制门禁，常量时间比较，不泄露 SK）。
package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSEOptions 是 SSE 传输的配置项。
type SSEOptions struct {
	// BearerToken 非空时对 GET /sse 与 POST /messages 强制 Bearer 门禁
	// （常量时间比较）；空 = 公开（零回归默认，本地/内网部署形态）。
	BearerToken string
}

// sseSession 是一次 GET /sse 连接对应的 MCP 会话：会话状态机（复用
// Server.handleMessage）与事件流出口。
type sseSession struct {
	id  string
	srv *Server

	// outCh 是 JSON-RPC 响应的事件流队列（SSE message 事件经它下发）。
	outCh chan string
	// mu 保护 closed 标记与 outCh 入队（POST 侧 dispatch 与事件流侧并发）。
	mu sync.Mutex
	// closed 标记事件流连接已断开：后续 POST /messages 一律 404
	// （响应无处可投，拒绝幽灵会话）。
	closed bool
	// stateMu 串行化同会话的多条消息处理（会话状态机非并发安全）。
	stateMu sync.Mutex
}

// NewSSEHandler 构造 SSE 传输的 HTTP handler：
//
//	GET  /sse?sessionId=<id>（可选）— 建立事件流，首帧 endpoint 事件下发
//	                             /messages?sessionId=<sid>
//	POST /messages?sessionId=<sid> — 收 JSON-RPC 请求（202 Accepted），
//	                                 响应异步经会话事件流回 message 事件
//
// 认证：SSEOptions.BearerToken 非空时两端点强制 Bearer 门禁（401 空 body，
// 常量时间比较，防 token 枚举）；空 = 公开。
func NewSSEHandler(tools *ToolRegistry, opts SSEOptions) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		if !sseAuthorized(r, opts.BearerToken) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handleSSEStream(w, r, tools)
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if !sseAuthorized(r, opts.BearerToken) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handleSSEMessage(w, r, tools)
	})
	return mux
}

// sseAuthorized 校验 Bearer 门禁：token 为空 = 公开；非空则 Authorization 头必须
// 为 "Bearer <token>"（常量时间比较；未提供/错误统一 401 空 body，不区分）。
func sseAuthorized(r *http.Request, want string) bool {
	if want == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleSSEStream 处理 GET /sse：建立事件流并保持到客户端断开/服务器退出。
// 首帧下发 endpoint 事件（/messages?sessionId=<sid>，MCP SSE 传输规范）。
func handleSSEStream(w http.ResponseWriter, r *http.Request, tools *ToolRegistry) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE 不支持", http.StatusInternalServerError)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")

	// 会话注册（并发安全的全局表）：POST /messages 按 sessionId 定位。
	sess := &sseSession{
		id:    newSessionID(),
		srv:   NewServer(nil, nil, tools),
		outCh: make(chan string, 16),
	}
	registerSSESession(sess)
	defer unregisterSSESession(sess.id)

	// endpoint 事件：data 是 POST /messages 的路径（含 sessionId query）。
	writeSSEFrame(w, fl, "endpoint", "/messages?sessionId="+sess.id)

	ctx := r.Context()
	for {
		select {
		case msg := <-sess.outCh:
			if msg == "" {
				// 会话已终止（POST /messages 侧 close）——事件流随即结束。
				return
			}
			writeSSEFrame(w, fl, "message", msg)
		case <-ctx.Done():
			// 客户端断开：标记会话终止，后续 POST 404。
			sess.mu.Lock()
			sess.closed = true
			sess.mu.Unlock()
			return
		}
	}
}

// writeSSEFrame 写一条 SSE 帧（event: <name>\ndata: <payload>\n\n）并刷新。
func writeSSEFrame(w http.ResponseWriter, fl http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	fl.Flush()
}

// handleSSEMessage 处理 POST /messages：按 sessionId 定位会话，把 JSON-RPC
// 请求体喂给会话状态机，响应异步经会话事件流回 message 事件。
func handleSSEMessage(w http.ResponseWriter, r *http.Request, tools *ToolRegistry) {
	sess := lookupSSESession(r.URL.Query().Get("sessionId"))
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if closed {
		http.Error(w, "session closed", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSSEMessageBytes))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	// 202 Accepted：请求已接收，响应异步经事件流下发（MCP SSE 传输规范）。
	w.WriteHeader(http.StatusAccepted)

	go sess.dispatch(r.Context(), body)
}

// maxSSEMessageBytes 是 POST /messages 请求体上限（单条 JSON-RPC 帧；防超大 body）。
const maxSSEMessageBytes = 1 << 20 // 1 MiB

// dispatch 在独立 goroutine 处理一条 JSON-RPC 消息，响应经事件流回下发。
// 坏帧不崩会话（回 ParseError 后继续），与 stdio 传输语义一致。
// 会话状态机跨消息共享（initialize/initialized/shutdown 持久），同会话的
// 多条 POST 经 stateMu 串行化（单用户单会话，简单正确——与设计文档一致）。
func (s *sseSession) dispatch(ctx context.Context, body []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			// 防御：分派 panic 不应崩掉事件流 goroutine/进程。
			_ = s.enqueueEncoded(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: CodeInternalError, Message: fmt.Sprintf("internal error: %v", rec)}})
		}
	}()
	var req request
	if uerr := json.Unmarshal(body, &req); uerr != nil {
		// 帧损坏：回 ParseError（id 为 null），与 stdio 语义一致。
		_ = s.enqueueEncoded(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: CodeParseError, Message: "Parse error"}})
		return
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	// 复用 stdio 的会话状态机管线：writer 桥接到事件流（sessionWriter 逐行
	// enqueue，非阻塞——事件流慢/断时不阻塞 POST 侧）。
	disp := NewServer(nil, &sessionWriter{s: s}, s.srv.tools)
	disp.initialized = s.srv.initialized
	disp.shutdown = s.srv.shutdown
	stop, _ := disp.handleMessage(ctx, req)
	s.srv.initialized = disp.initialized
	s.srv.shutdown = disp.shutdown
	if stop {
		s.close()
	}
}

// sessionWriter 把 Server.writeMessage 的输出桥接到会话事件流：
// 逐行（'\n' 分隔的 JSON-RPC 帧）enqueue，非阻塞——事件流断开后 Write
// 返回错误终止 handleMessage（会话已关闭的静默终止，不阻塞 POST 侧）。
type sessionWriter struct {
	s   *sseSession
	buf []byte
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line == "" {
			continue
		}
		if err := w.s.enqueue(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// enqueue 把一条已编码的 JSON-RPC 响应（单行 JSON）排入事件流队列；
// 会话已关闭/队列满则丢弃（客户端断开后的响应无处可投，静默丢弃不阻塞）。
func (s *sseSession) enqueue(line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	select {
	case s.outCh <- line:
		return nil
	default:
		return io.ErrClosedPipe
	}
}

// enqueueEncoded 编码并 enqueue 一条响应（与 marshalMessage 同帧格式）。
func (s *sseSession) enqueueEncoded(v any) error {
	b, err := marshalMessage(v)
	if err != nil {
		return err
	}
	return s.enqueue(strings.TrimSpace(string(b)))
}

// close 终止会话：事件流 goroutine 读到空串后结束，后续 POST 404。
func (s *sseSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	select {
	case s.outCh <- "":
	default:
	}
}

// --- 全局会话表（进程内单实例；多实例需共享存储，后续扩展） ---

var (
	sseSessionsMu sync.Mutex
	sseSessions   = map[string]*sseSession{}
)

func registerSSESession(s *sseSession) {
	sseSessionsMu.Lock()
	defer sseSessionsMu.Unlock()
	sseSessions[s.id] = s
}

func unregisterSSESession(id string) {
	sseSessionsMu.Lock()
	defer sseSessionsMu.Unlock()
	delete(sseSessions, id)
}

func lookupSSESession(id string) *sseSession {
	sseSessionsMu.Lock()
	defer sseSessionsMu.Unlock()
	return sseSessions[id]
}

// newSessionID 生成 16B 随机会话 ID（hex；crypto/rand 极端失败时回退时间戳）。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b)
}

// ErrSSEUnavailable 是 SSE 传输不可用时的哨兵错误（预留：未来多实例部署形态）。
var ErrSSEUnavailable = errors.New("mcp: sse transport unavailable")
