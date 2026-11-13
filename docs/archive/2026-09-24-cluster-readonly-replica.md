<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 集群只读副本接入（11.11-③）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-cluster-readonly-replica.md` 已于 2026-11-13 归档。
> 配置项以 `docs/config.md`（`cluster.role` / `cluster.enabled`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
非主节点以**只读副本**接入：共享外部卷（只读）+ 写面门 + 读面全开。本设计是选主设计的「读面补充」
——明确哪些路由不设门、只读形态如何复用、写降级语义。

## 定案与取舍（仍有效）
1. **双层兜底（关键）**：路由门（WriteGuard，非主 → 503）+ 存储门（只读 FS 未注入 Writer ⇒
   `ErrReadOnly` → 503）。**最坏情况是「写被拒」，绝不是「写错地方」**——即使门漏网，存储层仍
   fail-closed（禁静默降级）。
2. **读写路由清单是精确边界**：口诀「路由语义改状态（文件/meta/配置）→ 设门；只读状态 → 不设门」。
   读面逐路由断言**不依赖 WriteGuard**（防过度设门破坏副本读）。
3. **一次性分享 token 副本一律 503**：读即消费依赖计数原子写，副本无法原子消费 ⇒ 防同一 token
   多副本重复读导致**超发**；普通 token 放行且**跳过**计数写（计数仅主节点累计）。
4. **读降级最小化**：共享 StateStore 不可达 ⇒ meta 类读 503，但**纯文件下载 200**（文件在共享卷，
   不依赖 meta）；审计写失败仅本地记日志（尽力而为）。

## 落地指针
`pkg/server/config/cluster.go`（`cluster.role=replica` / `ClusterRoleReplica`）、`pkg/leader`
（`WriteGuard` / `Authorize`）、`pkg/volume/federated`（未注入 Writer 恒 `ErrReadOnly` 的只读形态）；
路由门清单见 `pkg/server/routes.go` 与 `pkg/server/writeguard_test.go`。
