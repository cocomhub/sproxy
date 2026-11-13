<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# AI 文件洞察（11.9-⑤）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-file-insight.md` 已于 2026-11-13 归档。
> HTTP 契约以 `docs/api.md`（`POST /api/ai/summarize` / `/api/ai/tag`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
新增 `POST /api/ai/summarize?filename=`（文件 → LLM 摘要）与 `POST /api/ai/tag?filename=`
（文件 → 标签列表）。**非目标**：不做对话/多轮、不做多模态（图片视频不入 LLM 文本）、不做 RAG 组装。

## 定案与取舍（仍有效）
1. **共用网关但错误策略不同（关键取舍）**：与告警建议器复用同一 LLM 网关，但洞察是**用户显式触发**，
   失败语义从「回退固定模板」改为「**明确返回错误**（502）」——显式操作必须告知失败，不静默空结果。
2. **只抽样首 4KiB 文本**（与搜索索引抽样同源、经 at-rest 解密读）：非文本（抽样为空）→ 明确
   `ErrNotTextFile`（400）。成本有界；「语义只覆盖头部」的局限文档化。
3. **缓存键 `(owner, rel, kind)` + 源文件 mtime/rev**：mtime 未变 ⇒ 零 LLM 调用；文件变更 ⇒ 失效。
   落盘走加密卷（见 `ai-privacy` 归档）；缓存损坏 ⇒ 视为未命中重算（幂等）；**不缓存失败**。
4. **输出规范化是契约**：摘要截断 ≤2000 rune；标签按逗号/分号/换行切分去重、每个 ≤20 rune、≤10 个。
5. **零回归**：`notify.ai_insight.enabled` 默认 false → 端点 400，不暴露新面。

## 落地指针
`pkg/server/ai/insight.go`（`AIInsight.Summarize/Tag`）、`pkg/server/ai/insight_cache.go`
（`InsightCache`）；路由 `POST /api/ai/summarize`、`POST /api/ai/tag`（**仅隧道内层** localMux，
主 mux 未注册）；配置 `notify.ai_insight`（`pkg/server/config/ai.go`）。
