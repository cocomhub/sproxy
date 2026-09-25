// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp 实现 Model Context Protocol（MCP）服务器：手写 JSON-RPC 2.0
// 协议层 + 会话状态机 + 工具分派（FileClient 薄封装），供 AI CLI
// （Claude Code / Codex 等）通过 stdio 传输暴露 sproxy 文件能力。
//
// 设计约束（见 docs/designs/2026-09-24-mcp-server.md）：
//   - 不引入第三方 MCP SDK，纯标准库实现；
//   - v1 传输定单行 JSON + '\n' 分隔（stdio，Claude/Codex 兼容）；
//   - 会话状态机：initialize → notifications/initialized → tools/*；
//     未初始化即调用 tools/* 返回错误；
//   - 工具业务失败统一包进 JSON-RPC 内部错误（-32603）的 data.message。
package mcp

import (
	"encoding/json"
)

// ProtocolVersion 是 MCP 协议版本标识（MCP 规范 2025-06-18）。
const ProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// request 是 JSON-RPC 2.0 请求（json.RawMessage 承载 params 原始 JSON，
// 便于工具参数校验前保留原始形状）。
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response 是 JSON-RPC 2.0 响应。
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 是 JSON-RPC 错误对象（data 可为任意结构，工具业务错误放
// data.message）。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// marshalMessage 将消息编码为单行 JSON（v1 stdio 帧格式）。
func marshalMessage(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	return b, nil
}
