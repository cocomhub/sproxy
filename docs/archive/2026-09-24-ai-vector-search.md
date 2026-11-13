<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 向量索引 + 语义搜索（11.9-④）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-vector-search.md` 已于 2026-11-13 归档。
> HTTP 契约以 `docs/api.md`（`GET /api/search/semantic`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
搜索索引原本只有文件名子串与内容抽样词元（子串级），无语义能力。目标：在内容词元之上叠加
embedding 向量层，端点为 `GET /api/search/semantic?q=...`（余弦 top-k）；**词元子串匹配保留为降级路径**。

## 定案与取舍（仍有效）
1. **不做向量数据库、不引外部索引依赖**：向量存本地 at-rest 加密文件 + 内存暴力 top-k。
   规模上限是硬约束：1536 维 ≈ 6KB/文件、1e5 文件 ≈ 600MB —— 文档注明，超出建议关闭语义搜索。
2. **写路径同步只登记占位，向量由 worker 异步补齐**（rev 幂等；rev 未变不重 embed）；
   **写路径不因 embedding 失败而失败**。
3. **查询失败必须 fail-safe**：embedding 未装配/检索失败 ⇒ **回退关键词模式**（响应带
   `mode: keyword`，`score` 恒 0），**不返回 5xx**——这是对外契约（客户端据 `mode` 判断能力）。
4. **只对头部采样**（首 4KiB，与 `sampleTokens` 同源）：语义只覆盖文件头，局限文档化。
5. **外部依赖须锁测试**：OpenAI 兼容 `/v1/embeddings` 与 Ollama `/api/embed` 两种协议形状；
   Provider=disabled / APIKey 空 ⇒ 客户端为 nil（不启用向量层，零回归）。

## 落地指针
`pkg/aiembed`（`Client.Embed` / `EmbedBatch`、`Config`）、`pkg/files/vector_index.go`
（`VectorStore` / `SemanticSearchHTTP`）；路由 `GET /api/search/semantic`；配置 `notify.ai_search`
（`enabled` / `topk`）。
