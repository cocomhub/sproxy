<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 集群扩缩容管理（方案 A-⑤）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-cluster-scaling.md` 已于 2026-11-13 归档。
> 配置与端点以 `docs/config.md`（`cluster.*`）与 `docs/api.md`（`/api/cluster/nodes`、`/api/cluster/self`）
> 为准，本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
方案 A 的选主/StateStore 解决了「写面唯一 + 状态一致」，但「谁来当节点、怎么加入/退出」无管理面。
目标：扩容 = 加只读副本；缩容 = 节点下线（主优雅释放租约 → 从补位）。**非目标**：不做自动化编排、
不迁移文件本体（外部共享卷是前提）。

## 定案与取舍（仍有效）
1. **装配期 fail-fast 校验**：replica 必须「外部共享卷 + 非 local state_store」；master 若租约被持有
   ⇒ **启动失败**（**不降级**，防双主配置）；replica **意外** `TryAcquire` 成功 ⇒ 立即降级退出
   （双保险，防「配了 replica 却因 bug 抢到写」）。
2. **CAS 加入防身份冲突**：节点注册表 `nodes/<id>` 用 `CAS(joining→active)`，同 id 双进程并发加入
   恰一个成功（退化为 Put 覆盖即缺陷）。
3. **下线尽力而为**：先停写面（isLeader=false）→ 排空 → `Release` → `leaving`；Release/Put 失败仅
   Warn + 继续退出（靠 TTL / `last_seen` 过期收敛）。
4. **副本索引预热**（List("index/") → ReloadIndex）避免扩容后首搜全量 WalkDir 冲击外部卷；
   预热失败回退懒构建 + Warn（**不阻断启动**）。
5. `cluster.enabled` 缺省 false ⇒ 校验/注册表/预热/下线钩子全不装配（单节点逐字零回归）。

## 落地指针
`pkg/server/cluster_scaling.go`（`NodeRegistry`、`handleClusterNodes`、`handleClusterSelf`）；
路由 `GET /api/cluster/nodes`、`GET /api/cluster/self`；配置 `cluster.{enabled,node_id,role}`。
