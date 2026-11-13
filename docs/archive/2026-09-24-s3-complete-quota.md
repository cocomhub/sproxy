<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# S3 CompleteMultipartUpload：配额记账 — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-s3-complete-quota.md` 已于 2026-11-13 归档。
> 配额语义与状态码以 `docs/api.md`（§协议挂载面 ·「S3 分块上传语义（Multipart）」）为准
> （现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

`complete` 拼接目标文件时**完全不经配额账本**：owner 配额超限也能落盘、卷容量池无感知、目标已存在
（覆盖写）时不释放旧占用（双计）。目标：把 S3 分块路径纳入与普通上传同口径的双账本。

## 定案与取舍（仍有效）

1. **账本口径必须与 `routeUpload`/files 域同源**：owner 配额 Scope + 卷容量池**双账本**，
   不另造一套「S3 专用账本」（否则对账必分叉）。
2. **预留生命周期完整**：`TryReserve` → 成功 `Commit` / 失败 `Release`，不留悬空 reserved；
   任一失败路径（含 ETag 校验失败）都要**双侧**回滚。
3. **覆盖写走差分**：`Adjust(prev, new)`——覆盖写不重复计入旧大小，缩小写要能把占用降下来。
4. **未装配配额 = 旧行为零回归**（nil scope / nil pool 直接跳过）。
5. **`507` 是 S3 语义下的唯一新状态码**（`InsufficientStorage`），文案区分「配额不足」与
   「卷容量不足」。
6. **行为变更是目的**：「超配额时 complete 成功」变 `507` 正是本片要堵的洞。

## 外部契约

- `507` 出现位置（**以代码为准，见下「实测差异」**）：part 上传与单次对象 PUT 的额度预检。
- part 文件占盘**不随配额记账**（parts 在 chunk 桶，不在 user 桶）——设计已披露为已知残余；
  实现后来用 chunk 桶 Scope 记在途 part（见下）。

## 落地指针

`pkg/server/s3/part_ledger.go`（`PartLedger`：`Reserve`/`Commit`/`ReleaseCommitted`/
`removePartReleasing`/`removeSessionParts`）、`pkg/server/s3/multipart.go`
（`CompleteSettlement` / `s3TargetPhysSize` / `CommitTarget` / `s3PartTotalSize`）、
测试 `pkg/server/s3_complete_hardening_test.go`。

## 实测差异（以代码为准）

- **预留时机被改了（最重要）**：设计写「complete 时双账本 `TryReserve`，超限 → 507」；实现把预留
  **前移到 part 上传**（每个 part 落盘前 `TryReserve` + 成功 `Commit`），complete 只做**转移结算**
  （把 parts 的账转到目标对象），**不再预留**。原因见 `CommitTarget` 注释：在 complete 再预留会
  **双计**同一批字节，使「配额刚好等于文件大小」的上传恒被 507 拒绝。
  ⇒ 现在 `507` 出现在 **part 上传**（与单次 PUT 一致），complete 自身不再返回 507。
- **入账尺寸取落盘真值**（`root.Stat`，加密卷即密文长度），而不是 parts 的明文合计——否则加密卷
  会少记 `物理−逻辑` 差额；`prev` 也取自 `root.Stat`（物理口径），两者必须同口径。
- 设计说「part 占盘不记账」是已知残余，**实现已把在途 part 纳入双账本**（同 partNumber 重传按差分
  释放）——残余闭合。abort/清扫删除 part 时**删成功才释放**（失败释放会少计）。
- 会话级锁（并发 complete/abort）仍未做，与 ETag 片同残余。
