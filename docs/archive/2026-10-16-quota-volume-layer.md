<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 配额卷层（Quota Volume Layer）— 归档

> **归档说明**：原文 `docs/designs/2026-10-16-quota-volume-layer.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/architecture.md` §配额 / §多卷布局、`docs/api.md` §`GET /api/quota`、`docs/cli.md` §quota。
> 模型规格见 `docs/designs/2026-10-14-quota-model-v3.md`（保留）。本文件只保留**根因 / 架构不变量 /
> 关键更正 / 被否决方案 / 落地指针**（分期与取证过程已移除）。

## 0. 根因（本设计要消灭的缺陷类）
记账是**横切关注点**，却被实现成 **N 个调用点各自的义务**，且「逻辑↔物理」换算这个义务**无法从类型系统发现**：
| 漂移类 | 机制 | 实证 |
|---|---|---|
| **① 单位不一致** | 每条写路径各自 `TryReserve/Commit`，尺寸取它**天然知道**的值（声明值/`LogicalSize`/`userVisibleSize` ⇒ **逻辑**），而磁盘真值是**物理** | 备份/S3 multipart complete/跨卷 move/sync pull —— **4 例同形态** |
| **② 释放 ≠ 结算** | 结算与释放是**两个**调用点，必须人工保证同口径 | `prevCharge`/`chargedHint`/quarantine 改名/归档 `actual` |

**判据（可复用）**：凡「写经**加密入口** + 尺寸来自**逻辑**来源」⇒ 必然漂移；凡尺寸取自**磁盘真值**⇒ 天然同源。
**根因不是某处写错，而是换算义务落在了错误的高度。**

## 1. 架构（用户裁定方向）
- **配额层只包存储卷**：`用户/协议面（零配额记账）→ 封装卷（视图层，零配额代码）→ 存储卷（`QuotaVolume`）`。
  wrapper 只做变换；`QuotaVolume` 每用户 Scope + 整卷 Pool；足迹 = 存储卷自报（天然物理）。
- **核心不变量**：*配额层与「真实字节」之间**没有变换层*** ⇒ 它看到的尺寸**天生是物理**，**无需知道卷是否加密**
  （加密发生在存储卷内部；`root.Stat` 返回密文长度）。
- **卷类型显式化**：`volume.Kind`（Storage vs Wrapper）——现状区分是**隐式**的（`enclosure.IsDeclared`）。

## 2. 关键更正（含上一版方案的硬错）
- 🔴 **配额层不能靠「包装 reader 计数」**：实测 `RootFS.WriteFile → writeUserVisible → 加密卷走
  OpenFileEncrypted` ⇒ **变换发生在 `storage.Root` 内部** ⇒ 配额层若包在 root **之上**，数到的只是**明文**。
  **修正**：配额层不是「字节计数器」，而是「**存储卷足迹的唯一消费者**」——写前问 `MaxPhysical`、
  写后/删前问 `Footprint`（两者都由卷实现）。（仅在配额层位于**变换之下**才能计数，但那会**丧失归属** ⇒ 不采用。）
- **P0-a 已撤销（2026-10-16 复核）**：`enclosure.SourceFS` 对本地源返回裸 `NewLocalFS` **是有意设计**（raw 视图），
  不是缺陷 ⇒ 不改。
- **P0-c 链构造收敛**：链构造有 8 处异构 ⇒ 收敛为唯一构造入口（`WrapStorageVolume`，R29 门禁）。

## 3. 收敛形态（已交付）
- 整卷容量记账今天散在 **5 个装配点**各自 `capacity.WrapBackend(be, counter)` ⇒ 漏一处即「静默无限额」。
  收敛为：在 **`registry` 侧定义消费者最小接口 `VolumeCounter`**（`TryAdd/Release/Used/Capacity`）——
  Go 接口是**结构性**的 ⇒ `capacity` 两种实现天然满足、**零 import**（避免 `registry` ↔ `capacity` 成环）；
  5 处改为**构造 `QuotaVolume` 并注入 counter**（同片删旧包装，否则双账本）；验收含「**恰好一次**」断言（写 N 字节 ⇒ `Used==N` 非 2N）。
- **联邦查询（片 5 读侧）**：`QuotaVolume.UserUsage/UserCapacity`（单卷每用户读侧）+ `Handlers.federatedUserQuota`
  （**唯一**汇总入口：只遍历 `AllowedVolumes` ⇒ ACL 收敛；无权威卷 `HasQuota=false` + 整体 `Partial=true`
  ⇒ **未知≠0**；上限 0 = 不限） + `GET /api/quota`（owner **恒取自认证**，不接受请求参数）。

## 4. 被否决方案（仍有效判断）
- 🔴 **§13「`QuotaVolume` 作为唯一记账权威（把记账字段搬进配额层）」——坚定否决**（2026-10-17 第 11 次前提证伪）：
  - 「物理尺寸公式散在调用方」✗ —— 已是最佳形态（`settleSize` **首选卷自报**
    `VolumePhysFootprinter.PhysFootprint`；`UploadRoute.PhysSize` 只是能力缺失时的回落）；
  - 「整卷权威按卷类型分治」= 缺陷 ✗ —— **结构性必然**（本地卷走 `Tenant.Root()`、外部卷走 `FS`；
    `CapacityFS`（包 `FS` 的装饰器）**结构上无法**覆盖本地卷 ⇒ 本地必须由 `UploadRoute.Pool` 记账）；
  - 「双账本靠调用点自律」✗ —— 已有**单一事实源** `UploadRoute.Commit`。
  ⇒ **改造成本高、答不上「不改会造成什么具体缺陷」** ⇒ 不做。
- 「给所有配额加落盘」（含本地卷）✗ —— 见 `docs/archive/2026-10-17-quota-ledger-persistence.md`。
- 「用文件锁 + 读改写做跨进程原子」✗（POSIX 锁跨节点不可靠）。

## 5. 落地指针
- `pkg/volume/registry/quota_volume.go`（`QuotaVolume`/`NewQuotaVolume`）、`pkg/volume/storage_volume.go`（`Kind`/能力接口）、
  `pkg/volume/capacity/`（`WrapStorageVolume` 唯一构造入口 / `phys.go` / `ledger.go`）、
  `pkg/server/quota_federated.go`、`pkg/server/volume_usage.go`、`cmd/sclient/quota.go`、`web/static/quota-format.js`。
