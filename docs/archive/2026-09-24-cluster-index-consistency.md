<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 集群索引一致性（方案 A-④）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-cluster-index-consistency.md` 已于 2026-11-13 归档。
> 配置项以 `docs/config.md`（`cluster.index_resync_interval` 等）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
多节点挂同一外部卷时，各节点内存索引各自增量、快照互相覆盖，副本 `built` 后永不重读 ⇒ 索引过期。
目标：主节点索引更新 → `Put(index/<owner>)`（StateStore）+ 事件广播；副本 Watch → 重载。

## 定案与取舍（仍有效）
1. **快照信封带 `rev` + `node`**：副本只在 `rev > appliedRev` 时重载——**rev 幂等是核心不变式**
   （旧 rev 信封到达必须忽略，不覆盖新索引）。
2. **信号只决定「何时去检查」，不决定「读到什么」**：一致性事实源是 StateStore 快照；Watch / SSE /
   resync 三路信号**共用单一 `ReloadIndex` 入口**，天然幂等。
3. **损坏/Get 失败 ⇒ 回退全量重建**（fail-safe：宁可重扫，不可用错索引）。
4. **写路径增量不逐个 Publish**：写置 dirty 标记，由周期 `saveAll` 或事件驱动显式 Publish
   （防重载风暴）。
5. **双轨切换可回滚**：单节点（未装配 `IndexSync`）恒走本地 `<tenant meta>/index/<owner>.json`
   （逐字零回归）；仅集群装配切 StateStore。
6. **跨 node 一律重载**（仅同 node 才比 rev）：换主即全量对齐一次——安全方向。

## 落地指针
`pkg/files/index_sync.go`（信封 + `IndexSync` + `ReloadIndex`）、`pkg/server/cluster_index_sync.go`
（Watch 循环 + resync 兜底）、`pkg/server/event_index_bridge.go`；配置 `cluster.index_resync_interval`。
