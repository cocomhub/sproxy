<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# AI 配额/审计（11.9-⑦）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-quota-audit.md` 已于 2026-11-13 归档。
> HTTP 契约以 `docs/api.md`（`GET /api/ai/quota`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
所有 `/api/ai/*` 与 AI 后台任务调用统一记账：审计事件（`ai.*` action 族）+ per-owner 配额
（超限 429）。**非目标**：不做计费/支付、不做跨节点配额一致、不做配额持久化（重启可清零重计）。

## 定案与取舍（仍有效）
1. **配额是前置门、审计是后置账**：超限**不调 LLM**，只记一条 `result=quota_exceeded`
   （防审计量放大）；每真实调用记一条审计。查询端点只读、不扣减。
2. **单节点 per-owner 内存计数器**（按本地日惰性重置；重启清零——文档注明）；配额**未启用 ⇒ 恒通过**
   （零回归）。
3. **tokens 是估算**（文本长度/4 近似）——文档明示「估算配额，非精确计费」。
4. **审计 action 前缀族（向后兼容约束）**：`action` 以 `.` 结尾 ⇒ 前缀匹配（`GET /api/audit?action=ai.`
   命中全部 AI 调用）；空 action = 全部、精确匹配语义**不得改变**——扩展前缀匹配必须同时保住
   「空/精确」旧行为。

## 落地指针
`pkg/server/ai/quota.go`（`AIQuota.CheckAndCharge/Usage`）、`pkg/server/ai/insight.go` 的 action 常量
（`ActionAIEmbed`/`ActionAISummarize`/`ActionAITag`）、`pkg/server/audit_handler.go`（action 前缀族）、
路由 `GET /api/ai/quota`；配置 `notify.ai_quota`。
