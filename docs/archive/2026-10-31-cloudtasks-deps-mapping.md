<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# cloudtasks 依赖映射：19 个装配依赖 → 8 字段 Deps（P1-c / S0）— 归档

> **归档说明**：原文 `docs/designs/2026-10-31-cloudtasks-deps-mapping.md`（557 行）已于
> 2026-11-14 归档。原文是 P1-c 的 **S0 映射分析（只读产出）**，其结论（S1–S4）已全部落地
> （#968–#975）。现行事实源见 `docs/architecture.md`「P1：装配层策略对象」与 `AGENTS.md`
> 能力矩阵（`pkg/server/cloudtasks`）。本文件只保留**收敛结果 / 门禁口径订正 / 无法覆盖的残余项 /
> 「必须逐字等价」的不变量 / 落地指针**；逐接口方法清单与逐行统计已随实现固化，不再保留。

## 结论摘要
两个 handler（`cloud_download_handler.go` + `cloud_archive_handler.go`）的 `h.*` 引用 **185 处 /
51 个符号**；其中真正的装配依赖 **19 个符号 / 130 处**（其余是文件内 helper）。收敛结果：
**19 → 8 个 `Deps` 字段**（7 个窄接口 + `*slog.Logger`）。`pkg/server` 42,568 → **31,221 行（−26.6%）**。

## 门禁口径订正（重要）
「`Deps ≤8`」是 P1 设计的**验收口径**，**不是现行门禁**——R52（`deps_field_cap_test.go`）的现行
上限是 **12**。因此切片**必须**在包内自补 `reflect.TypeOf(Deps{}).NumField() <= 8` 断言；
**禁止**为过门禁上调 `depsFieldCap`。

## 无法覆盖的残余项（及原因，仍有效）
| 项 | 处置 |
|---|---|
| **HTTP 响应/状态码** | 契约是「HTTP 面留装配层」；核心方法返回 `(result, error)` + 语义 `Kind`，装配层单点映射状态码（不把 HTTP 语义塞进领域包） |
| **`ActorFrom`** | ctx key 是 `pkg/server` 私有；装配层取 owner 后**显式传参** |
| **`drainAndVerifyBody`** | 请求体 EOF 哈希校验是 HTTP 中间件语义，留装配层 |
| **`msg*` 常量与响应结构** | HTTP 响应模型，留装配层；**文案逐字保留**（现有测试断言 message） |
| **`filesPhysFootprinter`** | 不是 `h.*` 形态但依赖 `Handlers`；**必须**并入 `Volumes.PhysFootprint`，否则 Deps 是 8、实际入边是 9（**隐藏耦合**） |
| **`archiveUsage` + `tenantMu`** | 包内状态，随迁由 `usageMu sync.Mutex` 保护；与 `Quota.BucketScope` 调用**不得嵌套加锁** |
| **`quota.Settlement`/`transfer.Execute`/`rootfs.*`/`archive.AddFileToTar`** | 领域/基础能力，直接 import 比注入更清晰（不是装配态） |
| **`groupErrorStatus` 字符串匹配** | 现状是脆弱契约；可保留（零行为变化）或换 typed error——**换必须逐条加测试锁原状态码** |

## 必须逐字等价的「不变量」（回归判据）
> 判定标准：迁移后调用**顺序、锁窗口、账本差分、错误文案/状态码**任一变化，都必须先有测试证明
> 是「有意变更」，否则视为回归。

- **配额结算（5 条路径，最易错）**：① 预留（`scope != nil` 走 `Settlement.ReserveWait`；
  `scope == nil && storageMgr != nil` 走 `WaitReserve(...DefaultWaitTimeout)`——**与 `h.quotaWaitTimeout`
  不对称是现状，不得「顺手统一」**；两者皆 nil ⇒ 不预留）；② 成功对账（scope 路径 `Stat` 失败
  ⇒ `Release` + `(0,true)`（文件已落盘，账本交周期扫描），**不是 Commit**；正常 `Reconcile` 失败
  仅日志、**仍 Commit**；回退路径 `Release` → `Stat` → `TryReserve` 失败 ⇒ 删归档 + 507）；
  ③ 失败回滚（O_EXCL 同名冲突与打包失败两条分支都必须释放预留，且**只释放一次**）；
  ④ 转存路径**不占 archive 桶**（写前 `ProbeReserve` 探针立即释放、不排队；写成功按**差分 `Adjust`**
  而非按 after 全额）；⑤ **每个预留句柄必须以 `Commit` 或 `Release` 收尾**。
- **`storageMgr` 回退**：只在 `scope == nil` 时使用；**禁止**恢复历史「双轨同时预留」。
- **卷路由 / ACL 顺序**：ACL 必须**早于**任何源文件打开（否则会泄漏 403 之外的错误面）；
  `externalVolumeTransferFS` 的返回语义（`ByName` 缺失但视图存在 ⇒ `fs` 非 nil 但 `ok=false`；
  `scheme == ""` 归一 `"local"`；`scheme == "secrets"` ⇒ 400）逐字保留。
- **写锁（`transferWriteLock`）**：键空间 `lockRelFor(owner, fsKey)` 剥 `<owner>/` 前缀，与
  HTTP 上传/删除/rename/DAV/S3 **同键**；顺序 `ReserveSpace`（可选，`ErrUnsupported` 跳过）→
  **取锁**（失败 409）；持锁窗口覆盖 `transfer.Execute` + 审计 + 配额结算 + 派生状态登记，
  **不得**在写成功后提前释放。

## 落地指针
`pkg/server/cloudtasks`（`deps.go`/`errors.go`/`download.go`/`group.go`/`archive.go`/`transfer.go`）、
`pkg/server/cloudtasks_wiring.go`（适配器）、`docs/architecture.md`「P1」。
