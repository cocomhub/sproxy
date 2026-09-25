// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp 实现 Model Context Protocol（MCP）服务器：手写 JSON-RPC 2.0
// 协议层 + 会话状态机 + 工具分派（FileClient 薄封装），供 AI CLI
// （Claude Code / Codex 等）通过 stdio 传输暴露 sproxy 文件能力。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ErrInvalidToolArgs 是工具参数校验失败（inputSchema required/类型不匹配）的哨兵
// 错误：handleToolsCall 见到它回 -32602（InvalidParams），其余工具错误回 -32603。
var ErrInvalidToolArgs = errors.New("invalid tool arguments")

// Server 是 MCP 会话服务器：以 r 为消息源、w 为响应出口（v1 stdio：
// 单行 JSON + '\n' 分隔），维护会话状态机并分派工具调用。
//
// 并发语义：Serve 串行读取并处理消息（单用户单会话），会话级串行锁
// 保证工具调用互不并发（简单正确，见设计文档）。
type Server struct {
	mu sync.Mutex

	r io.Reader
	w io.Writer

	initialized bool
	// shutdown 是 exit 通知后的终止标记：后续消息（除 exit 外）一律拒绝，
	// 且退出读循环。
	shutdown bool

	tools *ToolRegistry
}

// NewServer 创建一个 MCP 服务器。
//
// tools 为工具注册表（可为 nil——仅暴露协议层能力，用于单元测试的
// initialize/tools/list 状态机验证）。
func NewServer(r io.Reader, w io.Writer, tools *ToolRegistry) *Server {
	if tools == nil {
		tools = newToolRegistry(nil)
	}
	return &Server{r: r, w: w, tools: tools}
}

// ErrShutdown 是 exit 通知后的哨兵错误：Serve 读循环终止信号。
var ErrShutdown = errors.New("mcp: shutdown")

// Serve 读取并处理消息直到 EOF 或 exit 通知。
// 帧损坏返回 ParseError 后继续读下一帧（不崩进程）；EOF 正常返回 nil。
// exit 通知后返回 ErrShutdown。
func (s *Server) Serve(ctx context.Context) error {
	// v1 帧格式为单行 JSON + 换行 分隔（见 protocol.go 包注释）：按行读取、
	// 逐行 json.Unmarshal 是协议的天然实现——坏帧只影响本行，下一行不受影响
	// （这是设计文档选单行帧的原因）。若改用 json.Decoder 流式解码，语法错误时
	// decoder 处于可重放状态且其内部缓冲可能已提前读入后续行，无法可靠跳过坏帧，
	// 会退化为「坏帧 → 无限 ParseError 回包 + 死循环」。
	br := newBufReader(s.r)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			var req request
			if uerr := json.Unmarshal([]byte(line), &req); uerr != nil {
				// 帧损坏：回 ParseError（id 为 null）后继续读下一帧（不崩进程）。
				_ = s.writeMessage(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: CodeParseError, Message: "Parse error"}})
			} else {
				stop, herr := s.handleMessage(ctx, req)
				if herr != nil {
					return herr
				}
				if stop {
					return ErrShutdown
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// newBufReader 构造带缓冲的读取器（包级函数，便于测试注入）。
func newBufReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

// handleMessage 处理单条 JSON-RPC 消息，返回是否应停止读循环。
func (s *Server) handleMessage(ctx context.Context, req request) (bool, error) {
	switch req.Method {
	case "exit":
		s.mu.Lock()
		s.shutdown = true
		s.mu.Unlock()
		return true, nil
	case "notifications/initialized":
		// 通知（无 id）：幂等标记会话已初始化，不回响应。
		s.mu.Lock()
		s.initialized = true
		s.mu.Unlock()
		return false, nil
	}

	s.mu.Lock()
	shutdown := s.shutdown
	s.mu.Unlock()
	if shutdown {
		// exit 之后的任何消息（除 exit 自身，已在上方处理）一律拒绝。
		_ = s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidRequest, Message: "Server shutting down"})
		return false, nil
	}

	switch req.Method {
	case "initialize":
		return false, s.handleInitialize(req)
	case "ping":
		return false, s.writeResponse(req.ID, map[string]any{}, nil)
	case "tools/list":
		return false, s.requireInitialized(req, s.handleToolsList)
	case "tools/call":
		return false, s.requireInitialized(req, s.handleToolsCall)
	default:
		return false, s.writeResponse(req.ID, nil, &rpcError{Code: CodeMethodNotFound, Message: fmt.Sprintf("Method not found: %s", req.Method)})
	}
}

// requireInitialized 是 tools/* 的前置校验：未 initialize 即调用 → 错误。
func (s *Server) requireInitialized(req request, fn func(context.Context, request) error) error {
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if !initialized {
		return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidRequest, Message: "Server not initialized"})
	}
	return fn(context.Background(), req)
}

// handleInitialize 处理 initialize 握手：校验 protocolVersion，返回
// protocolVersion + capabilities.tools。
func (s *Server) handleInitialize(req request) error {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidParams, Message: "Invalid initialize params"})
		}
	}
	if params.ProtocolVersion != "" && params.ProtocolVersion != ProtocolVersion {
		return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidParams, Message: fmt.Sprintf("Unsupported protocol version: %s", params.ProtocolVersion)})
	}
	s.mu.Lock()
	s.initialized = true
	s.mu.Unlock()
	return s.writeResponse(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
	}, nil)
}

// handleToolsList 返回工具清单（{tools: [...]}）；无注册工具时返回空数组
// （JSON-RPC/MCP 客户端期望 tools 恒为数组，nil 会被序列化为 null）。
func (s *Server) handleToolsList(_ context.Context, req request) error {
	tools := s.tools.List()
	if tools == nil {
		tools = []ToolDefinition{}
	}
	return s.writeResponse(req.ID, map[string]any{"tools": tools}, nil)
}

// handleToolsCall 按 name 分派工具调用：参数校验失败 → -32602，
// 工具业务失败 → -32603（data.message 携带详情）。
func (s *Server) handleToolsCall(ctx context.Context, req request) error {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidParams, Message: "Invalid tools/call params"})
		}
	}
	if params.Name == "" {
		return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidParams, Message: "Missing tool name"})
	}
	handler, ok := s.tools.Lookup(params.Name)
	if !ok {
		return s.writeResponse(req.ID, nil, &rpcError{Code: CodeMethodNotFound, Message: fmt.Sprintf("Unknown tool: %s", params.Name)})
	}
	result, err := handler(ctx, params.Arguments)
	if err != nil {
		// 工具参数校验错误（-32602）与业务失败（-32603）区分：参数校验错误由
		// 工具层以 ErrInvalidToolArgs 哨兵返回（缺必填/类型错误），其余错误
		// 统一包进 -32603 的 data.message（设计文档错误处理节）。
		if errors.Is(err, ErrInvalidToolArgs) {
			return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInvalidParams, Message: err.Error()})
		}
		return s.writeResponse(req.ID, nil, &rpcError{Code: CodeInternalError, Message: fmt.Sprintf("Tool %s failed", params.Name), Data: map[string]any{"message": err.Error()}})
	}
	return s.writeResponse(req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": result}}}, nil)
}

func (s *Server) writeResponse(id json.RawMessage, result any, rpcErr *rpcError) error {
	return s.writeMessage(response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func (s *Server) writeMessage(v any) error {
	b, err := marshalMessage(v)
	if err != nil {
		return err
	}
	_, err = s.w.Write(b)
	return err
}
