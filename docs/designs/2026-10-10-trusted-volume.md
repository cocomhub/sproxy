# 可信卷（Trusted Volume）设计

> 日期：2026-10-10
> 状态：已实现（随 `feat/trusted-volume` 分支落地）
> 关联：`pkg/files/meta`（FileMeta 模型/计算/校验）、`pkg/volume/trusted`（装饰器 + meta 桶守卫）、`pkg/cloud/transfer.go`（转存读回校验）、`pkg/sync/equal.go`（通用比对）、`pkg/quota/staging_tracker.go`（本地 staging 配额）

## 1. 背景与目标

外部卷（baidupcs/s3/webdav/ftp/secretdata 等）**不承诺**跨信任边界的完整性：上传成功但内容损坏、读回校验和不可信、网盘静默截断。可信卷为每个文件生成**应用层独立的完整性证据** `FileMeta`，写入路径落盘隐藏 sidecar，读取路径（下载、云转存读回）逐分块比对，异常 **fail-closed**（不把损坏内容交给客户端/不把损坏产物记为成功）。

设计原则：

- **统一封装、不逐卷改方法**：任意 `syncpkg.FS` 经 `trusted.Wrap` 包一层即成为可信卷；新卷接入 = 包一层，零改卷本身。
- **卷自带 meta 则零封装**：实现 `meta.Provider` 的卷（如 secretdata 已有 shardseal.Meta）由卷自行提供/校验 FileMeta，`Wrap` 短路返回原 FS。
- **meta 恒生成**：`skip_verify` 只关读侧校验，不关 meta 生成（sidecar 是完整性证据；桶隔离/凭据保护不随之变化）。
- **meta 桶对业务层结构性不可达**：sidecar 与凭据（credentials.json）等内部数据同处 `meta` 功能桶，业务层 FS 门面经 `MetaBucketGuard` 从 keyspace 中移除该桶。

## 2. FileMeta 模型

`pkg/files/meta.FileMeta`（明文通用版，字段与 `cryptox/shardseal.Meta` 对齐，可互转）：

- `Size`、`TotalSHA256`、`TotalMD5`、`ChunkSize`、`Chunks[]{Index,Offset,Size,SHA256,MD5}`、`Name/MTime/CTime/MediaType/Extra/BaseVersion`。
- `Validate` fail-closed：版本/大小/总哈希/分块大小 + 分块连续覆盖 `[0,Size)` 无空洞；零字节文件放行（无分块）。
- 写侧 `Calculator` 流式累计（一次数据流两遍哈希齐算，零额外 I/O 遍数）；读侧 `VerifyReadSeeker` 流式逐分块校验。
- 分块大小：`ResolveChunkSize`（配置 `chunk_size`；`0` 按 `ChunkSizeForSize` 自适应 1MiB~32MiB；小于下界 `MinMetaChunkSize=1MiB` 钳到下界，防百万分块 meta DoS）。本地卷与外部卷装饰器共用同一口径。

## 3. sidecar 布局与配额

- sidecar 路径 = `meta.MetaPath(rel)`：把键的 `user` 桶段替换为 `meta` 功能桶（`volume.RebucketTo` 结构解析）→ `<owner>/meta/<rel>.meta`，与用户真实 `*.meta` 文件命名空间隔离。
- 本地卷 sidecar 经 `filesMetaPolicy` 写入并计入 **owner 的 meta 桶子 Scope**（写前预留/覆盖写按 prev 差分/删除联动释放）。
- 外部卷 sidecar 由 `TrustedVolumeFS` 经 `inner.WriteFile` 写入，计入**卷自身真实占用**（网盘容量自管，不进 owner 全局 Scope）。

## 4. 装配顺序与能力透传

```
be.FS() → trusted.Wrap（写 meta / 读 meta）→ trusted.Guard（meta 桶隔离）→ [StagingQuotaGateFS]
```

- **`trusted.Guard` 必须在最外层**（否则 Wrap 写 meta 会被守卫拒绝，meta 写入委托 raw inner）。
- 三层装饰器（Wrap / Guard / StagingQuotaGateFS）**必须透明转发全部可选能力接口**，否则上层 `fs.(能力)` 断言落在装饰器上恒失败，导致能力静默降级：
  - `meta.Provider`（`FileMeta`）、`metaExtraUpdater`（`UpdateMetaExtra`）→ 转存分块校验 / 下载校验 / damaged 标记；
  - `StagingQuotaExempt` / `StagingQuotaCapable` → s3 流式豁免 / baidupcs 自管；
  - `WriteIfAbsent` / `ReserveSpace` / `Mover` / `Copier` / `Linker` / `RangeReader` / `DirectURLProvider`。
- 各装饰器带**编译期断言**（`var _ 能力 = (*装饰器)(nil)`），防未来新增能力接口后遗漏。
- 能力未实现一律返回 `syncpkg.ErrUnsupported` 哨兵（调用方 `errors.Is` 识别后回落，与裸 FS 断言失败语义一致）。

## 5. 读路径校验（fail-closed 边界）

- **下载（本地卷）**：`filesMetaPolicy.VerifyDownload` 读 sidecar → `meta.VerifyReadSeeker` 包装 `ServeContent` 消费的 `SeekReadCloser`。
- **下载（外部卷，A/C 态服务端转发）**：`resolveExternalDownload` / `externalFSFor` 返回 `Guard(Wrap(fs))`，`fileMetaReaderFor` 取 `meta.Provider.FileMeta` → 同样包装 `VerifyReadSeeker`。
- **云转存**：`transfer.readbackVerify` 优先 `verifyByFileMeta`（目标卷 `meta.Provider` → 逐分块比对 meta，并与下载器权威 checksum 交叉比对）；无 Provider 回落整文件 sha256。
- `VerifyReadSeeker` 判据：
  - 构造时 `SeekEnd` 探测底层长度，与 `fm.Size` 不一致（截断/追加）→ 立即 fail-closed（不依赖「读到 EOF」——`ServeContent` 按 Stat 长度精确读满时不会产生底层 EOF 的那次 Read）；
  - 逐分块：仅从块首完整读满的块校验；越界（超出 `fm.Size`）**当次 Read 立即报错**；
  - 全量读（从 offset 0 读满）比对整文件 `TotalSHA256`；中段起始读（Range）不比对整文件哈希（避免误报）。
- `skip_verify=true` 显式跳过上述读侧数据校验（meta 仍恒生成）。

## 6. 写路径旁路覆盖

所有写/删/移动/复制入口都必须联动 meta，否则陈旧 sidecar 会使读校验恒失配、文件固化不可读：

| 入口 | 联动 |
|---|---|
| 普通上传 / 去重回退 / 去重硬链 | `filesMetaPolicy.WriteMeta`（到达即建） |
| 外部卷上传 / 转存 | `TrustedVolumeFS.WriteFile` / `WriteIfAbsent`（流式算 meta + 落盘） |
| S3 网关 / multipart | `s3WriteMetaAfter` / multipart complete 补建 |
| 分块上传完成 | `writeMetaSidecar` |
| 块级增量写（block_write） | `CloseBlockWrite` 原子落位后重建 meta |
| 跨卷 move | `moveMetaAfterVolumeMove`（目标重建 + 源清理 + 配额对称） |
| 跨卷 copy / 镜像 | `commitCopyResult` / 幂等命中 → `writeCopyMeta` |
| 备份到外部卷 | 目标 FS 经 `Guard(Wrap(fs))` |
| rename / move / copy / link / delete | `TrustedVolumeFS` sidecar 联动 + `ListDir/Stat` 隐藏 meta 桶 |

## 7. 本地 staging 配额

外部卷写本地暂存（如 baidupcs `staging-*`）会占本地磁盘。独立 staging Scope（`trusted_volume.staging_quota_bytes`，0 = 不限制但记账）+ `quota.StagingTracker`：写前 `ReserveUsage`（不足排队等待，ctx/超时中断）、写后释放。装配层对所有外部卷 `stagingQuotaFS`：显式 `StagingQuotaExempt`（s3 流式直传）跳过；`StagingQuotaCapable`（baidupcs）注入自管；否则 `StagingQuotaGateFS` 强制预留（fail-safe，新卷未实现也不漏）。

## 8. 已知边界与后续

- `FileMeta.Signature` 为预留字段（当前不写/不校验）——信任根由「meta 桶用户不可达 + 写路径独占」提供；启用完整 HMAC 需服务级签名密钥 + 全部写路径签名 + 读路径恒时校验。
- 整文件读回型交叉校验（并发覆盖错位检测）**仅本地卷执行**——远端卷回读 = 一次全量下载，成本不可接受；远端并发覆盖窗口由写路径独占 + 读路径校验兜底。
- `BucketOf` 桶名判定大小写敏感：大小写不敏感文件系统上 `META/...` 不被识别为 meta 桶（纵深防御缺口，当前无用户可控路径构造该键）。
- 崩溃恢复（统一恢复协调器 / staging 本地 meta 校验续传）为后续专题，见 `docs/archive/architecture-task-recovery.md`。

## 9. 验证

- 单元：`pkg/files/meta`（计算/校验/短读/长读/中段 Seek）、`pkg/volume/trusted`（Wrap/Guard/能力透传/生命周期）、`pkg/cloud`（转存分块校验/标记）、`pkg/sync`（Equal/staging gate）、`pkg/quota`（staging tracker）。
- 集成：`pkg/server`（外部卷下载校验、S3 gateway meta、卷间 move/copy）、`pkg/volume/ext/{baidupcs,s3}`（元数据/重试/并发）。
- 装配形状回归：断言 `Guard(Wrap(fs))` 仍满足 `meta.Provider` / `metaExtraUpdater` / `StagingQuota*`（防能力透传回归）。
