// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp 实现 Model Context Protocol（MCP）服务器：手写 JSON-RPC 2.0
// 协议层 + 会话状态机 + 工具分派（FileClient 薄封装），供 AI CLI
// （Claude Code / Codex 等）通过 stdio 传输暴露 sproxy 文件能力。
package mcp

import (
	"context"
	"encoding/json"
)

// ToolDefinition 是单个 MCP 工具的静态定义（tools/list 的清单条目）。
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// ToolHandler 是工具调用分派函数：接收 tools/call 的原始 arguments
// JSON（未解包时为空对象 {}），返回结果字符串（MCP text content）。
type ToolHandler func(ctx context.Context, arguments json.RawMessage) (string, error)

// ToolRegistry 是工具注册表：静态清单 + 按名分派。
type ToolRegistry struct {
	tools []ToolDefinition
	by    map[string]ToolHandler
}

// newToolRegistry 构建工具注册表（定义与分派同序登记）。
func newToolRegistry(entries []ToolEntry) *ToolRegistry {
	reg := &ToolRegistry{by: map[string]ToolHandler{}}
	for _, e := range entries {
		reg.tools = append(reg.tools, e.Definition)
		reg.by[e.Definition.Name] = e.Handler
	}
	return reg
}

// List 返回工具清单（tools/list 响应体）。
func (r *ToolRegistry) List() []ToolDefinition {
	return r.tools
}

// Lookup 返回工具的调用处理器；未注册返回 ok=false。
func (r *ToolRegistry) Lookup(name string) (ToolHandler, bool) {
	h, ok := r.by[name]
	return h, ok
}
