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

外部卷写本地暂存（如 baidupcs `staging-*`）会占本地磁盘。独立 staging Scope
（`trusted_volume.staging_quota_bytes`，0 = 不限制但记账；**独立记账**指与网盘
`owner_quotas` 分离，但**有意挂在全局池之下**——staging 实占本地盘，受
`max_storage_bytes` 约束，核心目的就是防本地盘打满）+ `quota.StagingTracker`：写前
`ReserveUsage`（不足排队等待，ctx/超时中断）、写后释放。装配层对所有外部卷经
`stagingQuotaFS` 处理：显式 `StagingQuotaExempt`（s3 流式直传）跳过；
`StagingQuotaCapable`（卷以 **per-instance** 状态自管，不得是共享单例）注入；
否则 `StagingQuotaGateFS` **per-request 包装**强制预留（fail-safe，新卷未实现也不漏）。

- baidupcs 的本地 staging 配额**由 caller 侧 per-request 门卫实例记账**，不再注入卷
  共享单例（`StorageFS` 是 backend 单例，per-owner 注入会跨 owner 串账）。
- 与**卷自身容量配额**（`syncpkg.ReserveSpace` / 卷容量 Pool）严格区分：后者管远端
  网盘容量，前者只记本地暂存字节。

## 7.1 卷级容量强制（外部卷 + 用户卷）

外部卷（配置卷 `volumes[]` + 用户卷 `/api/volumes/user`）的本系统可用限额在 **backend FS 层**
统一强制（`capacity.CapacityFS`）：写入累计、删除/覆盖/改名/服务端 Copy 释放，超限 fail-closed。
因此**凡经 `be.FS()` 的写路径**（HTTP 上传、云转存、同步 push、备份、服务端 Copy·Move）
都被同一卷级计数器拦截，保证「**所有用户在该卷的占用之和 ≤ 卷限额**」（跨 owner 共享）。

- 配置卷：`capacity.PoolCounter` 复用该卷 `vol_capacity` 池（与路由排序/指标/对账同源；
  `reserveVolume` 对外部卷改为**探测**，权威记账在 FS 层，防双计）。
- 用户卷：持久化 `VolumeCapacityCounter`（`<root>/<owner>/meta/volume/<name>.capacity`，
  非 `.json` 避开 store 扫描）；创建期与重启 restore 都包 CapacityFS。
- backend 级可选能力（HealthProbe/VolumeStatsProvider/Presigner/URLResolver）透明转发，
  内层未实现返回 `syncpkg.ErrUnsupported` 哨兵（消费方按「不支持」处理）。
- 装饰器链：`Guard(Wrap(CapacityFS(raw)))`；`trusted.Wrap` 下探透明装饰器（CapacityFS.Inner）
  判断卷自身是否自带 meta，避免把转发层误判为 Provider。
- **备份目标键空间归一**（FS-CORE-3）：备份源相对路径经 `prefixFS` 映射到目标卷桶键空间
  （外部卷 `<owner>/user/...`、本地卷 `user/...`）再进 `Guard(Wrap(...))`——否则 Guard 会把
  源里名为 `meta` 的目录误判为 meta 桶拒绝、sidecar 落到用户可见目录（不隐藏/不隔离）、
  且无 owner 前缀致跨 owner 同名相对路径互相覆盖；本地卷目标现在也包 Guard(Wrap) 建 sidecar。

## 8. 已知边界与后续

- `FileMeta.Signature` 为预留字段（当前不写/不校验）——信任根由「meta 桶用户不可达 + 写路径独占」提供；启用完整 HMAC 需服务级签名密钥 + 全部写路径签名 + 读路径恒时校验。
- 整文件读回型交叉校验（并发覆盖错位检测）**仅本地卷执行**——远端卷回读 = 一次全量下载，成本不可接受；远端并发覆盖窗口由写路径独占 + 读路径校验兜底。
- `BucketOf` 桶名判定**已大小写不敏感（2026-10-10 修复）**：`IsReservedBucketName` 用
  `EqualFold`、`BucketOf/RebucketTo` 统一归一小写，`META/`、`Meta/` 等变体均被识别为 meta
  桶并拒绝（`guard_fs_test.go` 已断言）。**残余**：Windows 反斜杠分隔符需 `guardPath`
  先归一（当前 in-tree 调用方均先经 `storage.NormalizeRemote`，纵深防御缺口，已记档）。
- 崩溃恢复（统一恢复协调器 / staging 本地 meta 校验续传）为后续专题，见 `docs/archive/architecture-task-recovery.md`。
- **远端卷写后交叉校验**（并发覆盖错位检测）仅本地卷执行（第 3 轮对抗评审 P2）：远端回读 =
  一次全量下载，成本不可接受；远端并发写同键的错配 sidecar 会固化为读失败（fail-closed）——
  经第 3 轮修复，backup 目标键空间已按 owner 归一（`prefixFS`），但 cloud 转存/上传到同一
  `<owner>/user/<rel>` 仍无按 rel 写锁；彻底解决需统一恢复协调器或按 rel 写锁，记档。
- **`syncpkg.BlockAccessor` 未转发**（第 3 轮对抗评审 P2）：四个装饰器不实现该可选接口，
  块级增量对装饰后的卷静默回退整文件复制。**不直接转发的原因**：装饰器无条件实现会让
  sync 引擎对不具备该能力的底层也走块级路径（`ErrUnsupported` 无法表达「未实现」），需引
  入 `Capability` 探测语义（后续专题）；当前无生产后端实现 BlockAccessor，无生产触发。
- **`/download/chunk` 部分 Range 不足一块时无逐块校验**（第 3 轮对抗评审 P2）：
  `VerifyReadSeeker` 只校验从块首完整读满的块；请求区间不覆盖任何完整 meta 块时本次响应
  不校验（剩客户端整文件 checksum）。彻底解决需把分块下载 offset/length 对齐 meta 块边界，
  记档。
- **baidupcs 本地 staging 峰值 ≈ 2×size**（第 3 轮对抗评审 P2）：`SyncFS.WriteFile` 落
  `staging-*` 后 `Storage.Put→stageUpload` 又复制一份 `stage-*`；gate 只按 1×size 预留。
  消除第二份复制需 `Storage.Put` 直接接受 staging 路径（改动 baidu 上传算法），记档。
  读路径（`Storage.Get` 整文件落本地）亦不受 staging gate 约束（同上）。
- **容量释放依赖 Stat**（第 3 轮对抗评审 P3）：`Delete/Rename/Move/Copy` 释放量取自
  `inner.Stat` 实测；Stat 瞬时失败时释放 0（计数偏高、仅对账/重启自愈）。
- **`FileMeta.Signature`**：同上行（预留字段）。

## 9. 验证

- 单元：`pkg/files/meta`（计算/校验/短读/长读/中段 Seek）、`pkg/volume/trusted`（Wrap/Guard/能力透传/生命周期）、`pkg/cloud`（转存分块校验/标记）、`pkg/sync`（Equal/staging gate）、`pkg/quota`（staging tracker）。
- 集成：`pkg/server`（外部卷下载校验、S3 gateway meta、卷间 move/copy）、`pkg/volume/ext/{baidupcs,s3}`（元数据/重试/并发）。
- 装配形状回归：断言 `Guard(Wrap(fs))` 仍满足 `meta.Provider` / `metaExtraUpdater` / `StagingQuota*`（防能力透传回归）。
