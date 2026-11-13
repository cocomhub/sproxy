<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 集群状态上移（11.11-②）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-cluster-state-migration.md` 已于 2026-11-13 归档。
> 配置项以 `docs/config.md`（`state_store.*` / `cluster.*`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
多节点挂同一外部卷时，各节点各写各的本地 JSON（凭据/dedup/分享/索引快照）⇒ **静默互相覆盖**。
目标：集群模式强制切共享 StateStore（主写副本读）。

## 定案与取舍（仍有效）
1. **领域包接口不动**：装配层适配器实现既有接口、内部委托 StateStore；未装配时走原实现
   （`state` 字段 nil = 本地 JSON，零回归）。
2. **不同状态不同失败语义（关键）**：
   - **凭据是权威 ⇒ fail-closed**：Get 值损坏 → 启动失败（禁静默重建）；`ErrKeyNotFound` → `(nil,nil)`
     对齐零凭据启动。
   - **索引是加速层**：Get 失败 → 回退重建 + Warn（不阻断读）。
   - **分享/审计尽力而为**：保留各 Store 现状语义。
3. **配额刻意不迁 StateStore**：高频内存账本，跨节点一致性由「写面唯一（LeaderElector）」承担
   ——这是明确决策，不是遗漏。
4. **集群模式但 StateStore 未装配 ⇒ 启动失败**（防「以为多节点一致、实际各写各的」）。
5. **可回滚**：双读单写（读旧 meta 回退 + 首写迁新路径）——每片独立可回滚，不改旧文件格式。

## 落地指针
`pkg/state`（`StateStore` 抽象 + 注册表 + `local`/`mongo` 后端）、`pkg/server/credentials`
（state-backed 凭据 store）、`pkg/server/share`（`StateBackedStore`）、`pkg/files/index_sync.go`
（索引快照后端）；配置 `state_store.{type,mongo.*}` + `cluster.{enabled,node_id,role}`。
