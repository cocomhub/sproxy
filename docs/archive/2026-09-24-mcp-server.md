<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# MCP server（S5-①，roadmap 11.9-①）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-mcp-server.md` 已于 2026-11-13 归档。
> 启动参数、工具清单与用法以 `docs/ai-integration.md` §1 为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
sproxy 已有 95+ HTTP 路由与 FileClient SDK，但无 MCP 协议层。目标：`sproxy-mcp` **独立二进制**，
把 sproxy 文件能力暴露为 MCP 工具给 AI CLI（Claude Code/Codex 等）。

## 定案与取舍（仍有效）
1. **独立二进制、对服务端零改动**：只依赖 `pkg/client` FileClient + `pkg/mcp`，**不 import 主服务
   装配**——主服务/既有 CI 零影响。
2. **手写 JSON-RPC 2.0，不引第三方 MCP SDK**（标准库优先，贴合依赖策略）。
   **v1 定单行 JSON + `\n` 分隔帧**（Claude/Codex stdio 均兼容），非 LSP 的 Content-Length 帧。
3. **协议版本协商 + 最小能力子集（tools）先行**：MCP 生态演进中，若生态强制 SDK 再评估；
   「不引第三方」是初始立场而非永久承诺。
4. **认证复用凭据 SK**（SproxySig）：SK 只在**本地签名、永不上线**；服务端零感知——MCP 工具 =
   FileClient 的薄封装。
5. **内存保护**：`read_file` 限长（如 1 MiB）、`write_file` 限长（超大走分块管线）；
   `delete` 需先 `stat` 取 checksum 防误删。

## 外部契约
stdio 帧格式（单行 JSON）、JSON-RPC 错误码（`-32700/-32601/-32602/-32603`）、工具清单与
`inputSchema`（参数对齐既有 HTTP 契约，如 `filename`/`subdir`/`checksum`）——见
`docs/ai-integration.md` §1.4。

## 落地指针
`pkg/mcp`（`protocol.go` / `server.go` / `tools.go` / `sse.go`）、`cmd/sproxy-mcp`
（`main.go` / `root.go`）。
