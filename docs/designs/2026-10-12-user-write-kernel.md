# 统一用户写内核 + S3/DAV 协议适配（2026-10-12 设计裁定）

> 背景：第 5 轮对抗评审「server 不应直操作底层 root」+ 用户「用机制避免反复打补丁、审视架构、
> 卷应作为 bucket 抽象」的定调。本文是**实施前裁定**：统一写内核（files.Service 单一写入口）+
> S3/DAV 变纯协议适配器（读写删列全委托 files.Service）。

## 1. 现状（实测盘点）

- 写面样板重复：S3 PUT（`s3PutWithQuota`/`s3PutReserve`/`s3PutObject`）、multipart complete
  （`s3CompleteMeasure`/`s3CompleteSettlement`/`s3CompleteWriteParts`）、DAV（`davMetaFS.WriteFile`）、
  版本恢复（`reserveRestoreQuota`/`restoreCopyVersionFile`/`settleRestoreQuota`）、云归档输出
  （`openArchiveOutput`/`cloudArchiveSettleQuota`）各持「锁 + 双账本 TryReserve/Commit/Adjust +
  sidecar WriteMeta + 原子写」手工样板 —— 任一面改语义其它面不同步 ⇒ 反复出补丁。
- `pkg/files.Service` 已是这些语义的**唯一聚合**：`WriteFile`（锁 + 自动路由 + 原子写 + 双账本 +
  sidecar + checksum + 台账 + mtime + 幂等/版本预处理），但只服务 HTTP 上传。
- S3/DAV 是「特化入口」而非「卷能力适配」：各自解析 tnt/root、各自直操作 root、各自读/删/列。

## 2. 目标形态（用户定调）

1. **统一写内核**：`files.Service` 新增 `WriteTarget(ctx, WriteTargetInput, src)` —— 锁/目标解析/
   记账/原子写/checksum/sidecar/台账全部内聚，**参数化差异**：
   - `Volume`：显式目标卷名（S3 bucket=卷、DAV/版本/归档=默认卷；ACL 校验）或空=自动路由
     （HTTP 上传语义不变）；
   - `Verify`：是否比对客户端校验和（HTTP 上传/版本恢复=true；S3/归档聚合=false）；
   - `Exclusive`：O_EXCL 新建（归档输出）；
   - `NoLock`：调用方已持锁（multipart complete 会话内）；
   - `NoQuota`：账目已由调用方结算（版本恢复/complete 转移语义）；
   - `Mtime`/`ClientSize`/`ExpectedChecksum`/`Owner`/`RemotePath` 复用现有字段。
   - sidecar（meta 恒生成）**内置**为写成功副作用（!Exclusive 时；Exclusive 归档不建 sidecar——
     archive 桶自身语义）；错误返回 `files.HTTPError`（文案/状态码对齐上传）。
2. **S3/DAV 变协议适配器**：S3 GET→`OpenPath`、DELETE→`DeleteFile`、LIST→`List`、PUT/complete→
   `WriteTarget`；DAV 提供一层**轻 sync.FS 适配**（把 `files.Service` 能力映射 webdav.Handler 所需
   OpenRead/WriteFile/Delete/Rename/MkdirAll/Stat/ListDir），删 `davMetaFS`（其 sidecar 联动已
   被 WriteTarget 内置覆盖）。bucket 维度保持「只 user 桶」（卷=桶名映射到 user 桶 relativ；
   cloud/archive/version 桶暴露属后续能力面重构，另开评审）。
3. **卷作为 bucket 抽象**（远期，不在本次落地）：`pkg/volume.Location{Volume,Bucket,Path}`
   + `FSPath()` 已是原料；未来 S3 bucket 可映射任意卷桶、DAV 可挂任意卷视图 —— 本次把
   「写入口唯一 + 协议适配层」先立起来，能力面重构在此之上做。

## 3. 实施顺序（todo #34→#40）

1. 本设计文档（§1-2）定稿。
2. `WriteTarget` 内核（锁→目标解析→记账→原子写→checksum→sidecar→台账，参数化）。
3. S3 PUT/complete 迁入（`Verify=false`；complete `NoLock+NoQuota` 保留在途记账转移语义）。
4. DAV PUT/MKCOL、版本恢复、云归档输出迁入。
5. S3 GET/DELETE/LIST + DAV 读删列全委托；DAV sync.FS 适配层（删 davMetaFS）。
6. HTTP 上传 WriteFile 重构为 WriteTarget（自动路由 `Verify=true`，幂等/版本预处理保留）。
7. 验证：全量测试 + 变异（去 sidecar/去锁/去记账/回 root 直写各自红）+ -race + lint/archcheck/
   deadcode/check-format → PR → CI 全绿 → squash 合并 → 清理。

## 4. 行为变更（开发期允许，记档）

- S3/DAV 写失败行为对齐上传：原子写（失败保留旧文件）替代 O_TRUNC 直写；文案/状态码对齐
  （409/507/400 统一 `files.HTTPError` 文案，S3 XML 响应体保持协议形态、状态码对齐）。
- S3 PUT 不再在 `s3Handler` 单独持 txn 锁 —— 统一内核持 upload 锁（同键空间，等价）。
- DAV 的 `davMetaFS` 删除（sidecar 由 WriteTarget 内置；webdav.Handler 经 sync.FS 适配直连）。
- 归档输出 O_EXCL 保留（`Exclusive=true`），不建 sidecar（archive 桶非 user 语义）。
- 版本恢复：`NoLock+NoQuota`（恢复前已做版本备份锁/记账），WriteTarget 只负责落盘+sidecar+台账。
