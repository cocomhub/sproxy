<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# LeaderElector 选主（集群化方案 A 配套）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-leader-elector.md` 已于 2026-11-13 归档。
> 配置项以 `docs/config.md`（`cluster.*`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
多节点挂同一外部卷时，各节点写本地 meta JSON（checksum/dedup/分享/配额）会静默互相覆盖；写面副作用
无唯一执行者。目标：**写面节点唯一**——仅主节点可写，从节点只读服务、写请求 503。

## 定案与取舍（仍有效）
1. **写面门 fail-closed（关键）**：`ErrLeaseLost`（续租失败被他人接管）⇒ **立即降级只读**，
   **禁「继续写」的宽限窗口**。网络分区按「续租失败 → 让位」收敛到新主（避免双主）。
2. **本地 flock 单节点恒主零回归**：无竞争者 `TryAcquire` 恒 true，与「无选主」行为等价。
   **不做「Windows 回落恒主」**（那会在多节点 Windows 部署下不互斥）——用 `LockFileEx` 实现等价排他，
   双平台同语义。
3. **写面清单设门、读面不设门**：从节点写 503 + `Retry-After`（客户端可重试/重定向到主）；
   写面转发不在本期范围。
4. **引导走抽象（门禁）**：R23 禁非测试源码直接 `flock`，统一走 `LeaderElector`。
5. **自动补位**：`RenewLoop` 以 `ttl/3` 续租、失败退避重试 `TryAcquire`；状态翻转可观测
   （日志 + metrics）。
6. **参数 fail-fast**：`leaseID` 为空 / `ttl <= 0` ⇒ 拒绝（防「无限期持有」的隐式语义）。

## 与设计稿的差异（避免误读）
- 设计稿的 `state_store.leader.{enabled,lease_ttl,renew_interval}` 配置**未实现**：装配走
  `cluster.node_id` + `cluster.role`，本地 flock elector 租约 TTL 硬编码；单节点/多节点本地卷场景
  以 flock 互斥。
- `MongoLeaderElector`（租约文档 + TTL index）作为**库**落在 `pkg/state/ext/mongo`，装配层
  **未启用**（当前只装配 `LocalLeaderElector`）。

## 落地指针
`pkg/leader`（`LeaderElector` / `LocalLeaderElector` / `WriteGuard` / `ErrLeaseLost`）、
`pkg/state/ext/mongo/leader.go`、`cmd/sproxy/root.go`（`setupClusterWriteGuard`）、
R23 门禁 `internal/archcheck/flock_gate_test.go`；配置 `cluster.node_id` / `cluster.role`。
