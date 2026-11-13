<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 事件流 AI 流水线（11.9-③）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-ai-event-pipeline.md` 已于 2026-11-13 归档。
> 事件端点与事件体格式以 `docs/ai-integration.md` §3 与 `docs/api.md`（`GET /api/events`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
AI Agent（向量化、摘要、打标等异步流水线）需感知文件变更。现状无 `/api/events`，事件通道需新建
（进程内 EventBus + SSE）。**非目标**：不做事件溯源、不保证至少一次投递、不实现 AI 编排器本身。

## 定案与取舍（仍有效）
1. **事件只是「失效/入队信号」，事实源是文件本体 + StateStore 快照**：事件丢失由周期全量扫描 /
   Watch/resync 兜底；事件**不承担正确性**（这一步避免引入 at-least-once 语义与事件持久化的复杂度）。
   核心验收正是「不依赖事件通道也能收敛」。
2. **内部与外部分离**：内部消费走进程内 EventBus 前缀订阅（`AIEventConsumer`），外部编排器走
   SSE `GET /api/events`（受认证）——同一变更语义两个通道，互不干扰。
3. **消费幂等**：per-(owner,rel) 去重窗口（默认 5m）+ rev 幂等（`evt.Rev <= cached` 直接跳过）；
   `delete/rmdir` → 同步删除派生条目（不重算）；`rename` → 条目 key 迁移（不重算）。
4. **失败不静默**：worker 指数退避重试 3 次 → 终败落 audit 事件 + Warn；队列满 → 丢弃 + Warn
   （有界队列 + 周期扫描兜底）。
5. **零回归**：`notify.ai_events.enabled` 默认 false → consumer 不装配；单节点未装配 EventBus 时
   周期扫描兜底（可观测 Warn，不静默降级）。

## 外部契约
SSE 事件体 `{cursor, action, owner, rel, size}` + `Last-Event-ID` 回放（游标单调递增，缓冲
1000 条/owner；落后过多滚出缓冲则回放失败，客户端应全量刷新）——**对外契约**，见
`docs/ai-integration.md` §3 与 `docs/api.md`。慢消费者（chan 满）被断开、重连回放。

## 落地指针
`pkg/server/ai/events.go`（`AIEventConsumer` / `NewAIEventConsumer`）、`pkg/server/events.go`
（`EventBus` / `FileChangeEvent`）、`pkg/server/event_index_bridge.go`（事件 → 副本索引失效）、
路由 `GET /api/events`（`pkg/server/routes.go`）、配置 `notify.ai_events`（`pkg/server/config/ai.go`）。
残余（后续片）：enqueue 落审计、真向量/摘要 worker 的周期扫描兜底。
