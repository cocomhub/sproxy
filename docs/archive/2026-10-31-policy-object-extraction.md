<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 装配层策略对象抽取（P1）— 归档

> **归档说明**：原文 `docs/designs/2026-10-31-policy-object-extraction.md` 已于 2026-11-14 归档。
> **P1-a/P1-b/P1-c 均已落地**（`volume.Router` #960/#962、`quota.Settlement` #961/#964/#965、
> `cloudtasks.Orchestrator` #968–#975）。现行事实源见 `docs/architecture.md`「P1：装配层策略对象」
> 与 `AGENTS.md` 能力矩阵（`pkg/server/cloudtasks` 的 `Orchestrator`/`Deps` 语义已并入相关行）。
> 本文件只保留**为什么不再切包的判据 / 三个策略对象的不变量 / 禁止项 / 落地指针**。

## 为什么不再按文件切包（判据，仍有效）
实测判据（写入 `docs/architecture.md`）：`cloud`/`volume`/`files` 的装配依赖 **30+ 个**，而叶子包
0–8 个；抽出后必须在 `Deps` 里复刻 `Handlers` 能力面 ⇒ **耦合不降反升**。正确做法是把散落的
**决策逻辑**收敛成**有行为、有不变量、可单测**的策略对象，装配层只做「取参数 → 调策略 → 写响应」。

## 三个目标对象与不变量
### 1. `volume.Router`（卷路由与 ACL）
问题：`locateForRead`/`locateOwnerFile`/`locateOwnerFileLegacy`/`locateOwnerFileScan`/
`volumeAllowedFor`/`defaultVolumeAllows` 分散、同一语义多份实现、ACL 判定与定位交织。
不变量：**同一 `rel` 最多命中一个卷**（AD-4，多卷命中时默认卷优先）；`Locate` 不创建租户目录
（只读无副作用）；`Allowed` 对未装配卷集/非法 owner **fail-closed**。

### 2. `quota.Settlement`（配额结算协议）
问题：`Reserve → 写 → Reconcile → Commit/Release` 的调用顺序散落在云归档/卷迁移/版本保存/S3
complete 等处；顺序错/漏回滚会导致**配额泄漏或超卖**。
不变量：`Commit`/`Release` **幂等**；`Release` 后不得再 `Commit`；未 `Commit` 的 `Settlement` 在
`Close`/GC 时自动 `Release`（防泄漏）；状态机非法转换返回哨兵错误（**不 panic**）。

### 3. `cloudtasks.Orchestrator`（云任务编排）
问题：`cloud_download_handler.go` + `cloud_archive_handler.go` 依赖约 30 个 `Handlers` 能力。
不变量：`Deps` 收敛为 **6–8 个窄接口**（`TaskStore`/`SourceFS`/`TargetWriter`/`QuotaSettler`/
`VolumeRouter`/`AuditSink`/`Logger`）；**HTTP 面留在装配层**（ctx→actor、状态码映射不进领域包，
遵循 P1-c 裁决）；包内补 `reflect.TypeOf(Deps{}).NumField() <= 8` 断言。

## 禁止项（红线）
- **不得**为过门禁放宽 `depsFieldCap`（R52 现行上限 12，见 `docs/archive/2026-10-31-cloudtasks-deps-mapping.md`）。
- **不得**把策略逻辑留在装配层同时新增薄封装（同一语义只留一份实现）。
- 依赖注入**只走构造期**（AGENTS 7b），不替换包级变量；单测纯标准库、`t.Parallel()`、只绑 127.0.0.1。
- 新增能力包必须登记 `internal/archcheck/layers.go`（`Levels`/`Managed`/`ParentDomain`）。

## 落地指针
`pkg/volume/router.go`、`pkg/quota/settlement.go`、`pkg/server/cloudtasks`（`Deps` + `deps.go`）、
`docs/architecture.md`「P1：装配层策略对象」。
