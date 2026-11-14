<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 可信卷（Trusted Volume：完整性 sidecar + 元信息隔离 + 卷级容量）— 归档

> **归档说明**：原文 `docs/designs/2026-10-10-trusted-volume.md` 已于 2026-11-14 归档。
> 能力与用法见 `docs/architecture.md` §可信卷（数据模型/装饰器链/读写保证/卷级容量）与 `docs/config.md`
> §可信卷。本文件只保留**设计原则 / 不变量 / 装配契约 / 踩坑族 / 仍有效边界 / 落地指针**。

## 1. 目标与原则
外部卷（baidupcs/s3/webdav/ftp/secretdata 等）**不承诺**跨信任边界的完整性（上传成功但内容损坏、
读回校验和不可信、网盘静默截断）。可信卷为每文件生成**应用层独立完整性证据** `FileMeta`，写路径落
隐藏 sidecar，读路径（下载、云转存读回）逐分块比对，异常 **fail-closed**（不把损坏内容交给客户端、
不把损坏产物记为成功）。

- **统一封装、不逐卷改方法**：任意 `syncpkg.FS` 经 `trusted.Wrap` 包一层即成可信卷；新卷接入 = 包一层。
- **卷自带 meta 则零封装**：实现 `meta.Provider` 的卷（secretdata 已有 shardseal.Meta）由卷自提供/校验，`Wrap` 短路返回原 FS。
- **meta 恒生成**：`skip_verify` 只关**读侧校验**，不关 meta 生成。
- **meta 桶对业务层结构性不可达**：`MetaBucketGuard` 从 keyspace 移除 `meta` 桶（与凭据文件同处）。

## 2. FileMeta 与布局（不变量）
- `pkg/files/meta.FileMeta`（明文通用版，与 `cryptox/shardseal.Meta` 字段对齐可互转）：`Size`/`TotalSHA256`/`TotalMD5`/`ChunkSize`/`Chunks[]{Index,Offset,Size,SHA256,MD5}`/`Name`/`MTime`/`CTime`/`MediaType`/`Extra`/`BaseVersion`。
- `Validate` fail-closed：分块**连续覆盖 `[0,Size)` 无空洞**；零字节文件放行（无分块）。
- **sidecar 路径 = `meta.MetaPath(rel)`**：把键的 `user` 桶段替换为 `meta` 功能桶 → `<owner>/meta/<rel>.meta`，
  与用户真实 `*.meta` 文件**命名空间隔离**（不是用户目录内的 `.meta`）。
- **分块上限与 sidecar 读取上限（64MiB）自洽**：`ResolveChunkSize` 为保证「写出即可读」上抬档位
  （`MinMetaChunkSize=1MiB`，防百万分块 meta DoS）。
- 本地卷 sidecar 计入 **owner meta 桶子 Scope**；外部卷 sidecar 由 `TrustedVolumeFS` 写，
  计入**卷自身真实占用**（网盘容量自管，不进 owner 全局 Scope）。

## 3. 装配顺序与能力透传（契约）
```
be.FS() → trusted.Wrap（写/读 meta）→ trusted.Guard（meta 桶隔离）→ [StagingQuotaGateFS]
```
- **`trusted.Guard` 必须在最外层**（否则 Wrap 写 meta 会被守卫拒绝）。
- 三层装饰器**必须透明转发全部可选能力接口**（`meta.Provider`/`metaExtraUpdater`/`StagingQuota*`/
  `WriteIfAbsent`/`ReserveSpace`/`Mover`/`Copier`/`Linker`/`RangeReader`/`DirectURLProvider`），
  否则上层 `fs.(能力)` 断言落在装饰器上恒失败 ⇒ 能力**静默降级**；各装饰器带**编译期断言**防遗漏。
- **能力探测必须下探 `syncpkg.Innermost`**：装饰器无条件回显 ⇒ 直接对装饰层断言恒真（曾致
  `RangeReader` 整流回落成死代码、`Provider` 探测失败、staging 门卫恒跳过）。
- 能力未实现一律返回 `syncpkg.ErrUnsupported` 哨兵（`errors.Is` 识别后回落）。

## 4. 读/写路径保证（要点）
- **读侧 fail-closed 边界（唯一权威表述）**：sidecar **缺失** = **零校验放行**（读路径既不算 meta 也
  不校验——**不是**「重算一遍再校验」）；缺失记 **Debug**、存在但读失败/损坏/陈旧记 **Warn 或 fail-closed**，
  **绝不做静默降级**。`VerifyReadSeeker` `SeekEnd` 探测与 `fm.Size` 不一致（截断/追加）→ 立即 fail-closed。
- **写侧旁路覆盖**：所有写/删/移动/复制入口必须联动 meta（上传/外部卷上传/转存/S3/multipart/分块完成/
  块级增量写/跨卷 move·copy·镜像/备份/rename/delete）⇒ 否则陈旧 sidecar 使读校验恒失配、文件固化不可读。
- **本地 staging 配额**（`trusted_volume.staging_quota_bytes`）：外部卷写本地暂存独立 Scope + `quota.StagingTracker`，
  **有意挂在全局池之下**（staging 实占本地盘，受 `max_storage_bytes` 约束，防本地盘打满）；
  与**卷自身容量**（`syncpkg.ReserveSpace`/卷容量 Pool）严格区分（后者管远端网盘容量）。
- **外部卷写后交叉校验**（并发覆盖错位检测）**仅本地卷执行**（远端回读 = 一次全量下载，成本不可接受）。

## 5. 踩坑族（仍有效，最可复用）
- **术语漂移**：全仓 ~35 处注释曾写「读路径直算兜底」（暗示缺失 sidecar 仍被校验），与实现不符 ⇒ 统一改「读侧不校验」。
- **`BucketOf` 桶名判定大小写不敏感**（`EqualFold` + 统一归一小写）：`META`/`Meta` 变体均识别为 meta 桶并拒绝。
- **备份目标键空间归一**：备份源相对路径须经 `prefixFS` 映射到目标卷桶键空间再进 `Guard(Wrap(...))`，
  否则 Guard 会把源里名为 `meta` 的目录误判为 meta 桶拒绝、sidecar 落到用户可见目录、跨 owner 互相覆盖。
- **S3 网关 PUT 需同时预留 owner Scope + 卷容量池**（只留 owner Scope 可叠加突破 `vol_capacity`）。
- **`presign/complete` 曾绕过 meta + 容量**（直传与上传口径不一致）⇒ 已强制登记（客户端声明 size+sha256，
  「内容已在位」模式完成存在性/size 校验 + 双账本结算 + sidecar/checksum 台账）。

## 6. 仍有效边界（记档，未修）
- **`FileMeta.Signature` 预留字段**（当前不写/不校验）——信任根靠「meta 桶用户不可达 + 写路径独占」。
- **`syncpkg.BlockAccessor` 未转发**：四个装饰器不实现 ⇒ 块级增量对装饰后的卷静默回退整文件复制
  （无生产后端实现，无生产触发；需先引入能力探测语义）。
- **`/download/chunk` 部分 Range 不足一块时无逐块校验**（剩客户端整文件 checksum 兜）。
- **容量释放依赖 `inner.Stat`**：Stat 瞬时失败时释放 0（计数偏高，仅对账/重启自愈）。
- **容量记账并发**：`CapacityFS` 无自身锁，`stat→reserve→settle` 跨锁 read-modify-write（逻辑漂移有界、自愈；
  彻底解决需按 rel 串行化，会串行化远端写 I/O）。
- **无桶段键的 `MetaPath` 兜底**：键不含功能桶段时回落 `<rel>.meta`（in-tree 调用方均经 bucket 限定键，不可达）。
- **baidupcs 本地 staging 峰值 ≈ 2×size**（`staging-*` 后又复制 `stage-*`；gate 只按 1×size 预留）。
- **加密封容量**：~~低估~~ 已修为**物理口径**（`PhysFootprint`/`PhysBound`，写前按物理**上界**预留、
  写后实测结算；无能力卷回落逻辑字节零回归）。

## 7. 落地指针
- `pkg/volume/trusted/`（`Wrap`/`Guard`/`TrustedVolumeFS`）、`pkg/files/meta/`（`FileMeta`/`Calculator`/`VerifyReadSeeker`）、
  `pkg/volume/capacity/`（`CapacityFS`/`phys.go`/`ledger.go`）、`pkg/server`（`filesMetaPolicy`/`MetaBucketGuard`/绑定注入）、
  `cmd/sproxy`（装饰器装配）。
