<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 计量报告（per-owner 周期用量导出）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-usage-report.md` 已于 2026-11-13 归档。
> 端点与用法以 `docs/api.md`（§计量报告 usage）为准（现行事实源），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
`max_storage_bytes` 是**总量约束**，但「这个月每个 owner 用了多少」无处可查：既有指标是
进程内全局 atomic 计数（无 owner 维度）与带标签运维指标（volume/mesh/hub），`/api/stats`
只有全局瞬时快照。目标是补 per-owner 周期（日/月）用量导出，供配额审计/成本分摊。

## 定案与取舍（仍有效）
1. **不新建旁路台账**（关键决策）：能从既有结构增量派生就不另造账本——
   - 存储占用：优先**既有索引快照汇总**（owner 维度天然对齐）+ 与 checksum 台账交叉校验；
   - 传输字节/文件数/请求数：在既有 `Metrics` 上**加 owner 维度标签**（复用
     `labeledCounters` 模式，基数 = owner 数，受配置界定）。
   理由：旁路台账会与主账本长期漂移，且要多一份写放大。
2. **默认不启用**（`usage.enabled=false` 零回归）：不产生落盘与计数开销。启用后**两套计数并存**，
   既有全局计数仍保持「瞬时快照」契约，不改其语义。
3. **`usageStore` 有界**：内存保留 N 个周期 + 周期落盘快照（`<storage_root>/meta/usage/
   <owner>/<YYYY-MM>.json`，原子写），跨日/跨月轮转——不做无界内存累积。
4. **权限口径**：owner 只能查自己，管理员可查全部；未装配 → `400 usage 未启用`（不静默返回空）。
5. **不触碰配额执行路径**（413 行为不变）——计量是观测面，不是准入面。

## 外部契约 / 踩坑
- `format=csv|json`（默认 json）；CSV 响应带 `Content-Disposition: attachment` 文件名——
  脚本/报表按此解析。
- 数据源为**日桶累计**，跨日/跨月边界即轮转点；改轮转口径会改变历史报表可比性。

## 落地指针
`pkg/server/usage_store.go`（`usageStore` / `RecordUsage`）、`pkg/server/usage_report.go`
（report / summary handler）、`pkg/server/routes_ops.go` 的 `GET /api/usage/report` 路由登记、
`pkg/server/config` 的 `usage` 段。
