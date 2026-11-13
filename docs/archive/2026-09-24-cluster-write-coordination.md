<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 集群写面协调（方案 A-⑥）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-cluster-write-coordination.md` 已于 2026-11-13 归档。
> 事件端点与格式以 `docs/api.md`（`GET /api/events`）与 `docs/ai-integration.md` §3 为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
主节点写成功后没有广播，副本不知道文件变了 ⇒ 读面（search/list/download）持续返回旧数据。
目标：写成功 → 事件总线发 `file.change`（owner/rel/op/rev）→ 副本订阅 → 索引/缓存失效。

## 定案与取舍（仍有效）
1. **事件是「低延迟失效信号」，与 StateStore.Watch/resync 互补**：事件负责「及时」，Watch/resync
   负责「兜底」。**不保证至少一次投递**（失效是幂等操作）。核心验收：**不依赖事件通道也能收敛**。
2. **尽力而为，绝不阻断业务**：Publish 同步回调但 handler 轻量（只入队标记 + SSE 扇出）；
   handler panic 被 recover + 日志；SSE 慢消费者（chan 满）丢弃 + Warn。语义同审计——
   事件失败**不改变写路径的错误返回**。
3. **写面成功后发布**（业务落盘 + 台账先成功）；**读面不发布**。
4. **单节点零回归**：EventBus 未装配 ⇒ 写路径无任何额外调用（nil 判断先行）。
5. **一致性事实源是 StateStore 快照**，不是事件——事件只决定「何时去检查」。

## 与设计稿的差异（避免误读）
设计稿的 `cluster.events_sse` 开关**未落地**：`GET /api/events` 端点常装配（受既有认证中间件
保护），无需额外配置键。

## 落地指针
`pkg/server/events.go`（`EventBus` / `FileChangeEvent` / `Subscribe` / `Publish`）、
`pkg/server/event_index_bridge.go`（事件 → 副本 `ReloadIndex`）；路由 `GET /api/events`。
