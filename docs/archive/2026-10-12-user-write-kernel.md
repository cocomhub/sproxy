<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一用户写内核 + S3/DAV 协议适配 — 归档

> **归档说明**：原文 `docs/designs/2026-10-12-user-write-kernel.md` 已于 2026-11-14 归档。
> 能力与用法见 `AGENTS.md`（能力矩阵 `pkg/files`：`Service.WriteTarget`「用户内容必须走这里」）与
> `docs/architecture.md`。本文件只保留**根因 / 目标形态 / 参数化差异 / 行为变更 / 落地指针与分歧**。

## 1. 根因
写面样板**重复**：S3 PUT（`s3PutWithQuota`/`s3PutReserve`/`s3PutObject`）、multipart complete
（`s3CompleteMeasure`/`Settlement`/`WriteParts`）、DAV（`davMetaFS.WriteFile`）、版本恢复
（`reserveRestoreQuota`/`restoreCopyVersionFile`/`settleRestoreQuota`）、云归档输出（`openArchiveOutput`/
`cloudArchiveSettleQuota`）各持「**锁 + 双账本 TryReserve/Commit/Adjust + sidecar WriteMeta + 原子写**」
手工样板 —— 任一面改语义其它面不同步 ⇒ **反复出补丁**。
`pkg/files.Service` 已是这些语义的**唯一聚合**（`WriteFile`：锁 + 自动路由 + 原子写 + 双账本 +
sidecar + checksum + 台账 + mtime + 幂等/版本预处理），但**只服务 HTTP 上传**。S3/DAV 是「特化入口」
而非「卷能力适配」：各自解析 tnt/root、各自直操作 root、各自读/删/列。

## 2. 目标形态（用户定调）
1. **统一写内核**：`files.Service.WriteTarget(ctx, WriteTargetInput, src)` —— 锁/目标解析/记账/原子写/
   checksum/sidecar/台账全部内聚，差异**参数化**：
   - `Volume`：显式目标卷名（S3 bucket=卷、DAV/版本/归档=默认卷；ACL 校验）或空=自动路由（HTTP 上传语义不变）；
   - `Verify`：是否比对客户端校验和（HTTP 上传/版本恢复=true；S3/归档聚合=false）；
   - `Exclusive`（O_EXCL 新建）/ `NoLock`（调用方已持锁）/ `NoQuota`（账目已由调用方结算）；
   - `Mtime`/`ClientSize`/`ExpectedChecksum`/`Owner`/`RemotePath` 复用现有字段。
   - **sidecar（meta 恒生成）内置**为写成功副作用（`!Exclusive` 时；Exclusive 归档不建 sidecar——
     archive 桶自身语义）；错误返回 `files.HTTPError`（文案/状态码对齐上传）。
2. **S3/DAV 变协议适配器**：S3 GET→`OpenPath`、DELETE→`DeleteFile`、LIST→`List`、PUT/complete→`WriteTarget`。
3. **卷作为 bucket 抽象（远期，不在本次落地）**：`pkg/volume.Location{Volume,Bucket,Path}` + `FSPath()`
   已是原料；先立「写入口唯一 + 协议适配层」，能力面重构在其上做。

## 3. 行为变更（开发期允许，记档）
- S3/DAV 写失败行为对齐上传：**原子写**（失败保留旧文件）替代 O_TRUNC 直写；文案/状态码统一
  `files.HTTPError`（S3 XML 响应体保持协议形态、状态码对齐）。
- S3 PUT 不再在 `s3Handler` 单独持 txn 锁——统一内核持 upload 锁（同键空间，等价）。
- 归档输出 `Exclusive=true` 保留（不建 sidecar）；版本恢复 `NoLock+NoQuota`（WriteTarget 只负责落盘+sidecar+台账）。

## 4. 落地指针与分歧
- 落地：`pkg/files/write_ops.go`（`WriteTarget`）、`pkg/server/s3/server.go`（`s3PutWithQuota` → `WriteTarget`）、
  `pkg/server/dav/meta.go`（`davMetaFS` 写路径委托 `WriteTarget`）。
- **与设计的字面分歧**：设计称「**删 `davMetaFS`**，其 sidecar 联动已被 WriteTarget 内置覆盖」；
  实现中 **`davMetaFS` 保留**（`pkg/server/dav/meta.go`）——它已**改为委托 `files.WriteTarget`**
  （§2.2 意图的落地形态），而非被删除。原因是 webdav.Handler 仍需要一个 `syncpkg.FS` 门面
  （OpenRead/WriteFile/Delete/Rename/MkdirAll/Stat/ListDir）承接 GET/PROPFIND/MKCOL/MOVE 等面，
  `davMetaFS` 即该适配层。（原文「删除」是表述，能力目标已达成。）
