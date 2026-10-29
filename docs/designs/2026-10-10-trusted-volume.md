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
- **sidecar 缺失/不可读的读侧语义（唯一权威表述）**：读路径**既不算 meta、也不校验**——即
  **零校验放行**（不是"重算一遍再校验"）。可观测性要求：**sidecar 缺失**（存量/旁路写）按
  Debug（`warnDegradeDebug`，升级后存量数据量大，避免刷屏）；**存在但读失败/损坏/陈旧**则
  必须 Warn 或 fail-closed（见 C2 与 §8.9），绝不做静默降级。
  > 术语纠正（评审 ⑧，2026-10-12）：全仓 ~35 处注释/日志曾写「读路径**直算兜底**」，暗示读侧会
  > 重算 meta 从而"仍被校验"——与实现不符（易误导维护者以为缺失 sidecar 是安全降级）。已全量
  > 改为「读侧不校验（sidecar 缺失=零校验）」。

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
- **无桶段键的 `MetaPath` 兜底**（第 4 轮对抗评审，SUSPECTED）：`MetaPath(rel)` 对不含
  功能桶段的键（如 `docs/x`）回落 `docs/x.meta`——sidecar 会落在用户可见目录且
  `IsMetaPath` 判为非 meta。当前**所有 in-tree 调用方均经 bucket 限定的键**
  （`Location.FSPath` / `prefixFS`），不可达；未加 Guard 拒绝（会误伤合法部分路径枚举，
  如外部卷根 `<owner>` 单段）。彻底解决需在 MetaPath 返回 (path, ok) 并逐调用方适配。
- **容量记账并发**（第 4 轮对抗评审 P2）：`CapacityFS` 无自身锁，`stat→reserve→settle`
  跨锁 read-modify-write；同 rel 并发 写/删 可致 used 少计或虚高。计数器内部操作原子安全
  （无内存损坏），逻辑漂移有界、重启/对账自愈；彻底解决需按 rel 串行化（会串行化远端写 I/O，
  权衡后留专题）。
- **加密卷容量记账低估**（第 4 轮对抗评审 P1）——**仍开（需产品口径）**：`CapacityFS` 包在
  `SecretdataFS` 外层，只按逻辑 size 计；容器内 chunk/meta 写入与默认 1–2MiB meta pad 不进
  计数器（1KiB 文件实占 2–4MiB）→ 理论上限可被绕过约 10³。彻底解决需按底层实测字节记账
  （暴露真实 usage 或底层树求和）。**现状收敛路径**：`pkg/storage/capacity` 的
  `StorageManager.SetReconciler` 周期对账（按物理足迹修正 used）⇒ 漂移有界自愈；实时精度
  属产品决策（按实测记账会带来额外 Stat/扫描成本，且与「逻辑容量」的用户预期不一致）。
  **不盲改**：需先定「卷容量语义 = 逻辑明文 vs 物理占用」。
- ✅ **已修（2026-10-14，P0 收口）**：`presign/complete` 现**强制登记**——客户端声明 `size`+`sha256`，
  域侧经统一写内核「内容已在位」模式完成 存在性/size 校验 + **双账本结算** + sidecar/checksum 台账；
  直传与上传同口径，容量/配额不再旁路。**残余（明示）**：① 服务端不重算 sha256（重算需完整下载对象）
  ② 账本按新对象记账（prev=0）⇒ 若同路径此前已经内核写入并记账会重复计一次（卷池由周期对账收敛、
  owner 配额侧偏高；覆盖既有文件请走上传 API）。
- ~~**presign 直传绕过 meta + 容量**~~（第 4 轮对抗评审 P1）——**已修（2026-10-14）**：
  `presign/complete` 强制登记（客户端声明 size+sha256 → 存在性/size 校验 + 双账本结算 +
  sidecar/checksum 台账）。残余（明示，非本次范围）：① 服务端不重算 sha256（需完整下载）；
  ② 账本按新对象记账（prev=0，覆盖既有文件请走上传 API）。
- **第 5 轮对抗评审修复批次一（2026-10-11）**：
  * 外部卷「无 RangeReader → 整流回落」此前是**死代码**（装饰器无条件实现 `OpenRangeRead`，
    `AssertRangeReader` 恒真）→ `NewRangeSeeker` 判据改为下探 `syncpkg.Innermost`；
  * **加密卷坐标统一**：新增 `storage.Root.LogicalSize`（明文长度），`/api/files` 列表、
    `/api/files/stat` 的 `X-File-Size`、`/download/chunk` 的 `Content-Range`/截断判据一律
    报明文；`encReadCloser.Seek` 支持任意正偏移 `SeekStart`（`/download` 的非零 Range 不再 416）；
    `/download/chunk` 先定分块边界再开句柄（越界回 416 而非 500）；缺文件回 404；
  * `/download` 校验失败改为 `panic(http.ErrAbortHandler)` 主动断连 + `success=false` 指标 + Warn
    （对齐 S3 网关 GET），不再静默截断却记成功；
  * 读校验降级可观测：sidecar 缺失记 Debug（升级后对所有存量文件成立，避免刷屏）、
    读失败/畸形记 Warn（本地与外部卷两条路径）；
  * **`pkg/syncexec` 补 sidecar 联动**：pull/both 本地目标改为「租户根 user 桶视图 × trusted.Wrap」
    （`userBucketFS` + `newLocalPullFS`）→ 覆盖写重建 sidecar、不再留陈旧 sidecar 致文件永久
    不可下载；`cmd/sproxy` 的 baidupcs 同步载体工厂补 `Guard(Wrap(...))`（push 到外部卷也生成 sidecar）；
  * **S3 网关单对象 PUT 补卷容量池预留**（此前只预留 owner Scope，可叠加突破 `vol_capacity`）；
  * **presign/complet 端点 owner 收敛（预存安全缺口）**：`presignOwnerKey` 把请求路径重绑到
    调用者 `<owner>/user/<rel>`，越权（他人 owner / 非 user 桶）→ 403；此前服务端凭据可对
    任意键签发 PUT/GET（且直传对象落在裸 key、文件 API 根本看不到）。
- ~~**回收站仅默认卷可见**~~（第 4 轮对抗评审 P1）——**已修（#830，2026-10-19）**：软删写
  home 卷 trash；`GET /api/trash` 缺省汇总全部本地卷（每条带 `volume`）、`?volume=` 过滤；
  restore 缺省跨卷定位；empty 支持 `volume=` / `all=1`（无参仍只清默认卷，破坏性默认不扩大）。
  CLI `sclient trash list/restore/empty` 已接入；Web UI 回收站按卷展示。

### 8.1 第 6 轮（B/C/D 面）修复与残余（2026-10-11）

**已修**：

- **备份 manifest 隔离（P1）**：manifest 由 `<目标卷>/user/backup/manifest.json`（**用户 API
  可读写**）迁到独立 **`backup` 功能桶**（本地卷 `<租户根>/backup/manifest.json`；外部卷
  `<owner>/backup/manifest.json`，经 `pkg/storage` `featureBuckets` 登记）。此前用户可覆写/
  伪造 manifest → 增量备份**静默跳过**（漏文件）且覆盖同名用户文件；`backup.Run` 新增
  `Options.ManifestFS/ManifestRel` 承载该命名空间，manifest 增 `checksum` 自校验（不符 → 全量
  重扫），handler 新增**同根守卫**（源/目标同一租户根 → 400，防自备份回灌源）。
- **guardPath 拒盘符前缀**（`C:/x`、`c:foo`）：守卫自身 fail-closed，不再依赖内层
  `SanitizeRelPath` 兜底（纵深口径统一；实测原本不可逃逸）。
- **目录级 Rename/Move 迁移 sidecar 子树**：`meta/<from>/**` → `meta/<to>/**`（trusted 装饰器
  `migrateMetaSubtree` + WebDAV 面 `davMetaFS.migrateMetaSubtree`）——否则新位置逐分块校验静默
  失效、旧子树成孤儿白占 meta 桶配额。
- **未知 size 超大流分块档位**：`ChunkSizeForSize(<=0)` 由 1MiB 抬到 32MiB（未知大小的流无法事后
  重分块；1MiB 时 >~341GiB 即超 sidecar 上限）；`meta.Marshal` 对超 `maxMetaChunks` 的 meta
  **拒绝序列化**（写侧提前失败 → 不落必被读侧拒绝的 sidecar）。
- **云转存复用 per-(owner,rel) 写锁**：`Options.TargetWriteLock`（装配层注入
  `filesRuntime.Acquire`，与 HTTP 上传/删除/改名/DAV/S3 同一键空间）；抢不到 → 可重试目标卷
  异常（编排层退避重试）。此前转存不经 `pkg/files` 写路径且远端卷写后不回读交叉校验 →
  跨写面同 rel 并发写可产出「主文件与 sidecar 错位」的固化状态（永久 fail-closed）。
- **MINOR**：`UpdateMetaExtra` 补 `ensureMetaDir`（provider 短路场景 `meta/` 目录可能不存在）；
  `Options.Extra` 改为**深拷贝**（`cloneExtra`，嵌套 map/slice 不再与配置共享底层数组）。

**未修（已实测/定性，设计级）**：

- **卷容量池不含 meta 字节**：`filesMetaPolicy.writeMetaCore` 与 S3 PUT 的 sidecar 走
  `storage.Root` 直写，只入 owner meta 桶 Scope，**不入卷容量池**（`capacity.CapacityFS` 只见经
  卷 FS 链的写）。修它需先定义 `rel → 卷` 的归属解析（原本由 `pkg/files` 路由持有，meta 策略层
  不感知），属装配语义改造；缺口有界（sidecar ≤ 64MiB/文件，且受 meta 桶 Scope 约束）。
- **外部卷 meta/owner 口径**：外部卷 `vol_capacity` 由 `CapacityFS` 权威记账，`owner_quotas`
  不适用（设计如此）；「meta 不入 owner Scope」是既有口径而非缺陷。
- **跨写面配额「双计」不成立**（实测）：S3 PUT 走 `root` 直写，owner Scope 单账本；不存在叠加
  双计。
- **`staging_quota_bytes` 默认 0**：per-owner staging Scope 无自身上限（但 staging 字节仍计入
  `globalPool` 的 `max_storage_bytes` 全局上限）；默认值属运维口径，保持 0（不限）并文档化。
- **读侧不做 MD5 交叉校验**：SHA-256 是完整性原语；`TotalMD5` 是网盘兼容字段（secretdata 等
  合法留空，语义可能不同源），比对它只增误报面不增安全。
- **`MetaPath` 无桶段回落**（`rel + ".meta"`，sidecar 与用户文件同目录、不隐藏）：仅当键空间
  缺 `<bucket>` 段时发生，而所有装配点（`prefixFS` / `bucketRel` / `ResolveLocation`）都保证桶段
  存在，属不可达边界。
- **`direct_link: true` 明文外部卷 302 直链绕过服务端校验**：卷配置项（默认 `false` = 私密 + 服务端
  转发），管理员显式开启即声明放弃服务端校验，属知情选择（已在 `config.example.yaml` 说明）。

### 8.2 第 7 轮（C 面 CRITICAL）修复（2026-10-11）

- **C1 同步载体缺前缀视图（P0 数据面）**：`cmd/sproxy` 的 baidupcs 同步工厂补
  `syncpkg.NewPrefixFS(fs, "<owner>/user/")`（与 HTTP 上传/转存/备份同一装配形状）。
  此前引擎的用户相对路径直达外部卷根：① 与 HTTP 面键空间不一致；② `meta.MetaPath` 无桶段
  回落把 sidecar 写到**用户可见**键（与用户真实 `x.meta` 互盖、`IsMetaPath` 不过滤 →
  同步引擎把它当用户文件：push 误删、pull 下载成垃圾）；③ 下载侧按 `<owner>/meta/...`
  找不到 → 零校验。同时把 `MetaPath` 无桶段回落改为 **`meta/<rel>.meta`（fail-closed）**：
  本地卷（FS 根 = 租户根）正是正确落点，共享/外部卷桶段判为 meta → 业务层守卫拒绝写入
  （不再污染用户键空间）。`PrefixFS` 提升为 `pkg/sync` 单一实现（全能力转发，纳入 R21 门禁），
  `pkg/server` 的 prefixFS 改为其别名。
- **C2 sidecar 损坏静默 fail-open**：`filesMetaPolicy.VerifyDownload` 现在区分「无 sidecar」
  （放行 + Debug）与「有 sidecar 但读失败/超限/解析失败」（**fail-closed 拒绝**）。
- **C3 Range 中段零校验**：`verifyReadSeeker` 增「先验后发」（中段起始块 seek 回块首读满整块
  比对，不一致则本批字节都不下发）与「EOF 补验尾部半块」；此前 `skipCur` + `!fromStart` 双短路
  使「起点非 0 且不覆盖整块」的 Range（单块/粗分块文件尤甚）零校验。
- **C4 302 直链绕过校验**：`externalStateFor` 仅在「显式 `skip_verify`」或「出口卷
  （302 二次进入由 `egress_forward=1` 强制转发，校验仍在服务端）」时允许直链；其余强制服务端
  转发并**告警一次**（禁静默降级）。

### 8.3 第 8 批（M1/M2/M5/M7/M8 + 相关 MINOR）修复（2026-10-11）

- **M1 同步冲突解决写回联动 meta**：`writeConflictFile` 写回后调 `relinkConflictMeta`
  （重建 sidecar；失败则失效旧 sidecar → 读侧不校验）。此前解析结果覆盖 marker 内容而
  sidecar 仍描述 marker → 下载逐分块校验恒失配（fail-closed 固化不可读）。
- **M2 转存写锁键归一**：新增 `Handlers.transferWriteLock`（`lockRelFor` 剥 `<owner>/` 前缀）
  并注入 `cloud.Options.TargetWriteLock`。此前转存用卷 FS 键（共享卷含 owner 前缀）、
  HTTP 上传用域侧 rel → 两写面锁**互不可见**，同 rel 并发写产出主文件/sidecar 错位固化。
- **M5 staging 独立根池**：`stagingQuotaScope` 改挂 `stagingPool`（容量 = `staging_quota_bytes`，
  0 = 不限），不再挂 `globalPool` 之下——慢速外部卷上传长期占暂存不再抬高 `max_storage_bytes`
  的全局 usage、误拒无关 owner 的正常写。**运维影响**：staging 不再计入全局上限，物理盘保护
  需显式配置 `trusted_volume.staging_quota_bytes`（建议按本地盘余量设置）。
- **M7 生产组合链贯穿测试**：新增 `pkg/server/trusted_chain_integration_test.go`——
  `Gate ∘ Guard ∘ Wrap ∘ CapacityFS` 全链 WriteFile → staging 预留对称 → `meta.Provider` 读
  sidecar → `UpdateMetaExtra` → 逐分块校验通过 + 篡改 fail-closed；并断言 `Innermost` 下探
  未被装饰层污染（此前的「静默降级」类回归只靠单包测试，编译与单测都拦不住）。
- **M8 aux 装饰器能力透传**：`davMetaFS` 补齐全部可选能力转发（写面能力**带 meta 重链**：
  `Move/Copy/Link/WriteIfAbsent`；读/元信息能力透传，未实现返 `ErrUnsupported`）并登记 R21；
  `pkg/syncexec` 的 `userBucketFS` 删除，改用 `syncpkg.PrefixFS`（单一实现、全能力转发）。
- **MINOR**：`UpdateMetaExtra` 失败改 Warn（m2）；`trusted_volume_test.go` 陈旧「disable 开关」
  注释修正（m3）；`TransferFSFor` 卷元缺失分支补 `Wrap`（m4）；`guardPath` 盘符判据改**按段**
  （`a/C:/x` 不再漏判，精确到「整段恰为 `<字母>:`」以免误伤合法含冒号文件名）；baidupcs
  `StorageFS.WriteFile` 路径校验改**逐段**拒空/`.`/`..`（此前只拒前缀 `../`，中段穿越透传远端）。

**仍未修（下一批，已记档）**（**历史记录**：最新状态见 §8.8 收口表）：M3（baidupcs staging 峰值 2×：`SyncFS` 落 `staging-*` 后
`Storage.Put` 再拷一份）、M6（远端 md5 非权威时跳过相等断言——现为「Warn + 接受」，评审主张
先 rapidupload 刷新重验）、M9（S3 multipart 崩溃遗留孤儿 parts 无清理；本地备份未走卷容量池
账本；外部卷写后交叉校验被 `isLocal` 门跳过；`skip_verify` 缺启动期显式告警）、m1（
`MetaBucketGuard.Inner()` 逃生口需「仅限能力探测」门禁）、m5（`TransferFSFor` 第三返回值语义 /
`DisableDoubleDownloadVerify` 规格-实现缺口）、m6（S3 multipart `UploadRetries` 在 TeeReader
非 Seeker 下恒失效）、m7（staging `ReserveUsage` 每轮 spawn select goroutine；baidupcs 单测 5s
等待）、m9（meta tmp 孤儿仅下次写清扫；`verifyWriteMeta`/`UpdateMetaExtra` 本地两遍整文件读回）。

### 8.4 第 9 批（M9d/m1/m7/m5 与 M9-MINOR 归并）修复（2026-10-11）

- **M9d `skip_verify` 装配期告警**：新增 `warnIfVerifySkipped(cfg, log)` 并在 `RegisterRoutes`
  装配时调用——`trusted_volume.skip_verify=true` 是可信卷承诺的降级开关（读侧不校验 / 302 直链
  前置 / 外卷写后不回读），必须启动即可见（此前只有配置注释）。测试含**接线断言**（防
  「helper 有测试但没人调」）。
- **m1 `Inner()` 逃生口门禁（R22）**：新增 `internal/archcheck/inner_escape_gate_test.go`——
  全仓非测试源码里 `.Inner()` **调用**只允许出现在 `pkg/sync/staging_gate.go`（Innermost
  下探）与 `pkg/volume/trusted/trusted_fs.go` 两处，新增调用须登记并写明理由；正则区分
  「方法定义」与「单行函数体内调用」。
- **m7 staging 等待观察者收敛**：`StagingTracker.ReserveUsage` 改**懒起 + 至多 1 个**观察者
  goroutine（快路径零 goroutine；多轮等待仍只 1 个），并加 `watchers` 计数供测试断言——
  此前**每轮等待**各 spawn 一个（高竞争 churn + 野 Broadcast）。
- **m5 去掉 `TransferFSFor` 死值**：第三返回值「是否共享」在转存路径从未被消费（共享卷 owner
  前缀合并由 `volume.ResolveLocation` 唯一入口承担），删除以免误导调用方需感知共享性。
  **评审另一项 `DisableDoubleDownloadVerify` 在本仓无对应规格**（全仓仅出现在本条记档里），
  属评审输入串味，无需实现。

**仍未修（下一批）**（**历史记录**：最新状态见 §8.8 收口表）：M3（baidupcs staging 峰值 2×）、M6（远端 md5 非权威时先 rapidupload
刷新重验）、M9a（S3 multipart 崩溃遗留孤儿 parts 清理）、M9b（本地备份未走卷容量池账本）、
M9c（外部卷写后轻量交叉校验，现被 `isLocal` 门跳过）、m6（S3 multipart `UploadRetries` 在
TeeReader 非 Seeker 下恒失效）、m9（meta tmp 孤儿仅下次写清扫；`verifyWriteMeta`/
`UpdateMetaExtra` 本地两遍整文件读回）。

### 8.5 第 10 批（M9c/M6/M9a/m6）修复（2026-10-11）

- **M9c 远端卷写后轻量交叉校验**：`verifyWriteMeta` 的远端分支不再直接 return，改走
  `verifyWriteMetaRemote`——① `Stat` 实测 size 必须等于 `meta.Size`（零额外流量）；
  ② 底层实现 `RangeReader` 时读**末块**（≤32MiB）重算 sha256 与 meta 末块比对。任一不一致
  → 删 sidecar（读侧不校验/逐分块校验），绝不留错证据；能力不足则保留（读路径校验不变）。
  消灭了「同一写路径本地卷有校验、远端卷完全没有」的承诺不一致。
- **M6 远端 md5 非权威先刷新重验**：`putCheckAfterUpload` 在 `MD5Unreliable` 时先经
  `renewAuthoritativeMD5`（rapidupload 秒传，命中会把远端 md5 刷新为权威整文件 md5）→
  刷新后正常做相等断言（不等则重传刷新 meta）；仍非权威才按「无校验能力≠损坏」接受并告警。
  此前直接接受，把「必须 md5 一致」在分片上传（>4MiB）路径架空。
- **M9a S3 multipart 孤儿会话清扫**：新增 `sweepStaleMultipart`（懒触发：每次
  InitiateMultipartUpload 顺带一次，零后台 goroutine），清理 meta mtime 超过
  `s3MultipartOrphanTTL`（24h）的会话及其全部 parts——客户端崩溃/断连未发 abort 的残留
  不再永久占 chunk 桶与配额。抽取 `staleMultipartSession`/`removeMultipartSession` 保持
  可读（gocognit）。
- **m6 S3 重试失败显式归因**：`putObjectWithRetry` 对「源非 io.Seeker / Seek(0) 失败」的
  fail-closed 早退路径把「UploadRetries 未生效」写进错误（并更新 `UploadRetries` 与
  `S3FS.WriteFile` 注释的前置条件）——可信卷写面读流是 `io.TeeReader`，该配置在其上本就不
  生效，此前只能看到裸 PutObject 错误，运维无法区分「重试已尽力」与「根本没重试」。

**仍未修（下一批）**（**历史记录**：最新状态见 §8.8 收口表）：M9b（本地备份写面未更新**卷级持久容量计数器**（用户已裁定：按**强一致**处理，下一提交）——需先定「本地卷
durable 容量账本」在备份路径的归属/API）、M3（baidupcs staging 峰值 2×）**已由 §8.6 的 staging 路径复用机制解决**（见 §8.8）、m9（meta tmp 孤儿仅下次写清扫；`verifyWriteMeta`/
`UpdateMetaExtra` 本地两遍整文件读回，需「stat/末块轻量判据替代整文件回读」的等价性论证）。

### 8.6 本地 staging 路径复用机制（2026-10-11）

**问题**：需要本地暂存的卷（baidupcs）此前落两份——调用方 `staging-*` 一份，卷内部
`stage-*/<basename>` 又拷一份 → 本地峰值 **2×size**，而 `staging_quota_bytes` 的 gate 只按
**1×** 预留（保护减半），且与「本地突发占用」的真实峰值不符。

**机制**（`pkg/sync/stagedpath.go`，两条等价协作路径）：

1. **能力接口**（可静态探测）：`StagedPathReuse.WriteFromStagedPath(ctx, relPath, stagedPath,
   size, mtime)` —— 卷声明「可直接消费调用方已落盘的本地文件」。契约：**只读** stagedPath
   （不移动/不删除；生命周期归调用方，实现方可能在返回前并发读它）。
2. **ctx 传递**：调用方经 `WithStagedLocalPath(ctx, path)` 发布已暂存路径，卷侧写面
   （`StorageFS.WriteFile`）自取复用——适合调用方是通用上传器、不愿引入额外接口分支的场景。

**落地**：

- `baidupcs.Storage.PutFromStagedPath(ctx, key, stagedPath, size)`：从**已存在**的本地文件
  单遍读算 md5/crc32 + 读回算 sliceMD5/分块 md5 列表（不新建副本），随后走既有
  「Stat 复核 + ETag 一致才成功」的有界重试主循环（复用语义不变）；
- `StorageFS.WriteFile`：ctx 有已暂存路径 → **零额外落盘**直接复用；否则本函数落一份
  `staging-*` 后同样以复用入口上传（不再由 Storage 内部拷第二份）；
- `prepareStagedForAdapter`：交给 adapter 的本地文件 basename 必须等于目标 basename
  （CLI 适配器按本地 basename 决定保存名，随机 basename 会 Stat 恒 NotFound）——不一致时用
  **硬链接**（零字节拷贝，峰值仍 1×）到临时子目录；跨卷/不支持硬链接才回落拷贝（此时才 2×，
  且仅限 CLI 场景，记 Warn）；
- 透明装饰器（`TrustedVolumeFS`/`MetaBucketGuard`/`CapacityFS`/`StagingQuotaGateFS`/
  `PrefixFS`/`davMetaFS`）**逐层转发**该能力并带 R21 编译期断言（新增 `StagedPathReuse`
  自动进入门禁检查集）；`StagingQuotaGateFS` 的复用路径**不重复预留**（数据已由调用方暂存
  且已被 gate 按 1× 预留）。

**验证**：`TestStorageFS_WriteFile_ReusesCtxStagedPath`（复用分支零额外落盘 + 只读契约）、
`TestStorage_PutFromStagedPath_NoPhysicalSecondCopy`（交给 adapter 的路径必须是暂存文件的
**硬链接**，在 adapter 内用 `os.SameFile` 判定——拷贝版会被 cleanup 删掉，事后扫描看不见）、
`TestStorageFS_WriteFile_SingleLocalStaging`；变异两处命中（去 ctx 复用分支 / 链接改拷贝）。

## 8.7 备份目标持久容量账本（强一致）与本地写后校验收敛

**M9b｜本地备份写面并入 durable 容量账本（强一致）**
- 缺口：本地卷目标的备份写面只记 owner user 桶 `Scope` 与目标卷容量池两本**内存**账，而
  `/stats` 与 `ScanAndRecalculate` 的权威账本是 `capacity.StorageManager` —— 无
  `vol_capacity` 的本地卷只靠周期扫描对账 ⇒ 两次扫描之间容量统计**低估**（备份字节不入账）。
- 修法：`backupQuotaFS` 增 `sm/cat`（本地卷目标在 Scope/卷池之外对声明 size 预留
  `TryReserve`；写成功按实际大小结算——账本无 `Adjust`，走 `Release(pre)+TryReserve(actual)`；
  写失败/取消回退；**三本账同进同退**，任一侧拒绝即整体拒绝并回退已成功侧）。覆盖写按
  `prev` 差分、`Delete`/`Rename` 对称释放；类别与用户文件写面一致（`CategoryChunked`），
  `/stats` 不新增口径；外部卷目标不参与本地磁盘账本。
- 验证：`TestBackupQuotaFS_DurableCapacityLedger`、`TestBackupAPI_LocalTargetDurableCapacityLedger`
  （端到端：备份后账本 = 实际字节，增量备份全 skip 不漂移）；变异 2 处命中。

**m9｜本地写后校验改有界轻量判据 + meta tmp 孤儿清扫扩面**
- `verifyWriteMeta` 本地分支原为**整文件读回**重算 `TotalSHA256`（每次写付一次全量读，
  6GiB 级写放大一倍）→ 与远端**统一**为有界判据：`Stat` size 必须等于 `meta.Size`；底层
  实现 `RangeReader` 时读**末块**重算 sha256 与 meta 末块比对。
  **如实声明放弃的检出**：同 size 且末块相同、仅中段被并发覆盖的错位（旧实现可捕获）——
  该场景由读路径逐分块校验兜底（读到被覆盖分块即失败），并发同 rel 写本身无锁语义。
- meta tmp 孤儿清扫：`filesMetaPolicy.sweepMetaTmp` 兼容两种命名（本文件的 `.tmp.<nano>`
  与 `LocalFS.writeFileAtomic` 的 `.tmp-*`——原只匹配点前缀，`LocalFS` 崩溃残留的
  `.tmp-*` 永不清理）；并在下载读路径 **sidecar 解析失败**时联动清扫一次（损坏 sidecar 的
  常见成因正是「写 meta 崩溃于 rename 前」，孤儿常驻占 meta 桶配额）。
- 验证：`TestTrustedVolumeFS_VerifyWriteMetaLocalLightweight`（中段篡改不检出 / 末块篡改
  与 size 不一致均失效 sidecar）、`TestFilesMetaPolicy_SweepMetaTmp`（两种命名清扫 +
  不误删同目录其他文件）；变异 2 处命中（去末块判据 / 去 `.tmp-` 分支）。

## 9. 验证

- 单元：`pkg/files/meta`（计算/校验/短读/长读/中段 Seek）、`pkg/volume/trusted`（Wrap/Guard/能力透传/生命周期）、`pkg/cloud`（转存分块校验/标记）、`pkg/sync`（Equal/staging gate）、`pkg/quota`（staging tracker）。
- 集成：`pkg/server`（外部卷下载校验、S3 gateway meta、卷间 move/copy）、`pkg/volume/ext/{baidupcs,s3}`（元数据/重试/并发）。
- 装配形状回归：断言 `Guard(Wrap(fs))` 仍满足 `meta.Provider` / `metaExtraUpdater` / `StagingQuota*`（防能力透传回归）。

## 8.8 遗留项收口状态（2026-10-11，随封装卷分支更新）

> §8.x 各轮的「仍未修」列表是**当时**快照（保留作审计轨迹），本节是唯一权威状态。

| 遗留项 | 状态 | 落点 |
|---|---|---|
| M2 转存写锁键归一 | ✅ 已修（§8.3） | `lockRelFor` + `transferWriteLock`；**文档措辞已修正**：锁覆盖 HTTP 家族（上传/删除/改名/分块 complete/跨卷 move），DAV/S3/备份/同步 push 不经该锁（真实风险有限，非「同一键空间全覆盖」） |
| M3 baidupcs staging 峰值 2× | ✅ 已修 | `SyncFS.WriteFile` 无条件走 `PutFromStagedPath`（硬链接复用，峰值 1×）；ctx/能力复用路径为**额外优化**（当前无仓内调用方，记档 P3） |
| M6 远端 md5 非权威 | ✅ 已修（§8.5） | 先 rapidupload 刷新重验，仍非权威才接受并告警 |
| M9a S3 multipart 孤儿 parts | ✅ 已修（§8.5） | `sweepStaleMultipart` + `s3MultipartOrphanTTL=24h` |
| M9b 本地备份持久容量账本 | ✅ 已修（强一致） | `backupQuotaFS` 三本账同进同退（Scope + 卷池 + `StorageManager`） |
| M9c 远端写后交叉校验 | ✅ 已修，且**已统一为有界轻量判据**（含 `UpdateMetaExtra` 前置校验） | `metaContentMatches`：本地/远端同口径（Stat size + 末块 sha256；放弃「同 size 中段覆盖」检出，读路径逐分块兜底）。**第 7 轮残余 ⑤ 已收口**（2026-10-12）：`UpdateMetaExtra` 的**前置**校验此前被 `isLocal` 门整段跳过 → 远端并发覆盖只能靠写后判据发现（那时 meta 已污染）；现远端改用同一 `metaContentMatches` 前置判据，本地仍保留整文件 TotalSHA256 前置校验 |
| M9d skip_verify 启动告警 | ✅ 已修（§8.5） | `warnIfVerifySkipped` |
| m9 meta tmp 孤儿 / 整文件回读 | ✅ 已修，**并修正首版假修** | 清扫判定对齐真实生成端（`.<base>.tmp-*` 带前导点）；本地写后校验改有界轻量判据 |
| Windows 反斜杠分隔符 | ✅ 已修 | `guardPath` 拒绝 `\`/NUL 且 fail-closed（`guard_fs_test.go` 已断言）——§8 旧文「残余」描述作废 |
| 加密卷容量按逻辑长度记账（可绕过 ~10³） | ⚠️ 机制就绪、待接入 | `capacity.TrueUsageReporter` + `CorrectTo/CorrectFrom` 已落地；`secretdata`/封装卷待实现物理字节报告（内容分块+meta pad+parity） |
| 面级加密入口未接线（S3/DAV/分享/分块上传/块写） | ✅ 已修 | 用户可见读写唯一入口（`pkg/server/user_visible_io.go`）；DAV 收敛到 `NewRootFS`（同时修掉封印绕过）；分块上传 finalize 单遍加密；块写加密卷显式拒绝 |
| 封装卷「源目录禁止直连」 | ✅ 已修（机制级） | `storage.Root.SetSeals`（写恒拒/读 404 或只读透视）+ 装配接线 + 租户子根传播 + `enclosure.FS` 直链抑制 |
| 新装饰器漏转发可选能力（R21 只查硬编码清单） | ✅ 已修 | R23 自动发现门禁（`Inner()` 与「嵌入 FS/持有 inner FS 字段」两类）+ `quotaLocalFS` 能力补齐 + 处理层显式豁免 |
| egress/cluster 卷 sidecar 键空间未归一（下载**零校验**） | ✅ 已修（§8.10） | 出口卷自带 `meta.Provider`（经 remote `meta` 面）+ 持有侧用**自身可信栈**解析 sidecar——不再把 meta 桶语义塞进 remote 路径 |

## 8.10 集群出口卷的真实完整性校验（2026-10-12 落地，第 5 轮评审 P2-10）

**问题**：出口（egress）卷是**代理型卷**——它没有本地文件，sidecar 在**持有侧**的键空间里。
但出口侧装配链对它也套了 `TrustedVolumeFS`（因它不自带 `meta.Provider`），后者的 `FileMeta` 用
`inner.OpenRead(meta.MetaPath(rel))` 直取 → 键空间错位（`<owner>/meta/<rel>.meta` 被当作持有侧
**用户相对路径**；持有侧再经 `UserRel` 映射成 `user/<owner>/meta/...`）→ 恒 404 →
`externalDownloadSource` 走「sidecar 缺失」降级分支 → **egress 卷下载零校验**（可信卷承诺不成立）。

**根因（架构）**：把「声明 `meta.Provider`」当成了装配的必然结果，而不是**卷必须自己说清**的能力。
代理型卷无法用自己的键空间拼接去拿另一端的 sidecar，却因装饰器而被认定为「有校验能力」。

**处置（能力归属归位 + 协议面显式）**：

1. **`clusterFS` 自带 `meta.Provider`**（`pkg/volume/ext/cluster/fs.go`）：`FileMeta(ownerKey)` 先经
   `holderRel` 归一为**持有侧用户相对路径**，再交给注入的 `fetchMeta`。因自带 meta，`trusted.Wrap`
   保持原样返回（与 secretdata 同构：自带 meta 的卷自己管），不再叠加 `TrustedVolumeFS`；
   `Guard` 仍透传该能力（`TestEgressMetaCapabilityNotShadowed` 为结构回归锚点）。`fetchMeta` 为 nil
   时 **fail-closed**（不静默返回 nil 让调用方误当「无 sidecar」）。
2. **remote 只读面新增 `GET /remote/meta`**（`pkg/remote/client.go` 的 `Meta` + `pkg/server/remote_read.go`
   的 `handleMeta`/`delegateMeta`）：入参是**用户相对路径**（与 list/stat/download 同语义），
   sidecar 由**持有侧自身可信栈**解析。因此**凭证范围（PathPrefix）仍作用在用户路径上**，
   不引入「meta 桶可达」这一越权面（区别于「把 meta 键空间暴露给远端」并把凭证 scope 复杂化）。
3. **持有侧解析镜像自身校验口径**（`pkg/server/meta_remote.go`）：
   - 本地卷 → 读 `filesMetaPolicy.VerifyDownload` 同一键 `meta.MetaPath(user/<rel>)`；
     **加密卷跳过**（sidecar 描述密文、服务流是明文——同 `VerifyDownload` 的口径）；
   - 外部卷 → 经 `externalRemotePath` 给下载源注入的**同一 `meta.Provider`**（同键 `FSPath`）。
   故「出口侧校验用的元数据」==「持有侧自己下载时会校验的元数据」——不引入新的语义分叉。
4. **错误分类保留**：真·无 sidecar → 404 → 客户端 `errors.Is(os.ErrNotExist)` → 出口侧按
   「无校验能力」**预期降级**（Debug）；读取失败/畸形 → 非 404 → 出口侧 **Warn**（不静默当无校验）。
   响应体限读 `MaxMetaSidecarBytes()+1`（超限按读取失败，不截断成畸形 JSON）。

**测试**：`pkg/server/cluster_egress_meta_e2e_test.go`（真双端：内容一致 → 通过；**持有侧内容被篡改
（同 size、不同字节）→ 出口下载 fail-closed**；无 sidecar → 降级可读；能力不被 Wrap 遮蔽）、
`pkg/server/meta_remote_test.go`（本地/外部两分支 + 读取失败不伪装 404）、
`pkg/volume/ext/cluster`（holderRel 桥 + ErrNotExist 保留 + 畸形拒绝 + 装配接线）、
`pkg/remote`（op 往返 + 404 分类）。**变异 3 命中**（FileMeta 用未归一 key／持有侧不重桶／缺失不映射 404）。

**有意未做**：出口侧不去「猜」持有侧是否为加密卷——加密口径由**持有侧**决定并反映在它的回答里
（返回 404），出口侧只消费结论；否则出口侧需要复制一份持有侧配置，正是本项要消除的耦合。

## 8.11 容量/staging 记账残余收口（2026-10-12 落地，第 5 轮评审 r1-capacity F2–F5 + F7/F8）

| 项 | 严重度 | 症状 | 处置 |
|---|---|---|---|
| **F2** 写失败回滚假定「错误⇒未落盘」 | P2 | 远端后端（baidupcs/s3）无原子 rename，响应超时/连接中断时对象**可能已提交**；此前一律 `Release(reserved)` ⇒ 卷级 `used` 永久偏低（限额可穿透 ~单文件大小，不重启不自愈） | `CapacityFS.settleAfterWriteError`：写失败时 **Stat 复核实测值并结算到实测**（本地卷/未提交 → 结算回旧口径，不泄漏）；**Stat 无法复核 → 保留预留不清账**（宁可偏高，限额宁紧勿松）。`WriteIfAbsent` 的 `(false,nil)`（幂等命中，**明确的未落盘答复**）仍回滚预留。附带修 `countingReader` 对 `r==nil` 的 panic（仅声明长度的写） |
| **F3** `size<=0` 未知长度写完全不进 staging 记账 | P2 | 无 Content-Length / copyMeta 类：`StagingTracker.ReserveUsage(size<=0)` 是 no-op，gate 亦无写后结算 ⇒ 并发流式写可打满本地盘而配额不可见 | `StagingQuotaGateFS` 未知长度分支改 `reservingReader`：**每读取 n 字节即预留 n**（实测口径；`Read` 阻塞即背压），写完释放累计实测总量（`WriteFile`/`WriteIfAbsent` 同口径） |
| **F4** `StagingTracker` 丢失唤醒（空间已释放仍空等到 deadline ≤5s） | P2 | `TryReserve` 失败与 `cond.Wait` 之间未持锁，此间的 `ReleaseUsage` 广播被丢失；且每轮等待新起 goroutine 共用单次投递的 `timer.C` | `waitRound`：**持 `cond.L` 先 double-check `TryReserve`** + `ReleaseUsage` **持 `cond.L` 释放并广播** + 观察者**取 `cond.L` 后广播**（等待者持锁直至 `Wait` → 信号不可落在窗口里）。三处叠加后不存在可丢窗口 |
| **F5** `CorruptSnapshot` 仅 Warn（无可观测面） | P2 | 计数文件是占用量的唯一权威记录，损坏即 `used` 归零（限额被放宽一轮；反复损坏=反复放大），除启动日志无指标/健康项 | `capacity.CorruptSnapshotCount()`（进程级原子计数，累计点内聚在 `Load`/`NewPoolCounterPersistent`，含 `cmd/sproxy` restore 路径）+ `/metrics` 暴露 **`sproxy_volume_capacity_corrupt_total`**；启动 Warn 保留 |
| **F7** create 在校验名前用 `req.Name` 拼计数器路径 | P3 | 名字含分隔符/`..` 时 `Capacity.Load` 读到 meta 目录之外的 `*.capacity`（读失败直接 500；顺序/错误码不当） | `createUserVolumeHandler` 在拼路径前用**与 store 同一判定**（`storage.ValidSegmentName`）拒绝：非法名 → 400 |
| **F8** `Load` 的 capacity 一致性注释与实现不符 | P3 | 注释称「持久化 capacity 不一致会告警」，实现从未比较（文档/行为漂移） | 注释改为如实描述：快照 `capacity` 仅供追溯，**不参与比较、不改写传入值**（配置/请求为权威） |

**测试与变异**：`pkg/quota`（F4：`beforeWait` 钩子把「释放恰在 TryReserve 失败与 Wait 之间」确定化并断言 <1s 完成；去 double-check → 4s 超时红）、`pkg/sync`（F3：未知长度写期间配额在账峰值 == 实测字节数；回退透传 → 峰值 0 红）、`pkg/volume/capacity`（F2 三态：已提交→记 40 / 未落盘→回滚 / Stat 不可复核→保留；`nil` reader 不 panic；F5 计数）、`pkg/server`（F5 `/metrics` 计数 >0；F7 非法名 → 400，变异回退 → 409 红）。

**F6 用户卷上限继承（2026-10-12 落地，用户裁定）**：`capacity:0` 且未显式 `unlimited`
⇒ **继承同名卷已持久化的上限**；`unlimited:true` ⇒ 显式不限；`capacity>0` ⇒ 显式声明（可改大/改小并把
used 钳到新上限）。**生效上限创建期即落盘**（计数快照 + 卷元数据，否则「建卷零写入 → 删除 → 重建
`capacity:0`」时无快照可继承 = 绕过仍成立；元数据不落则重启 restore 用请求里的 0 抹掉继承值）。
语义唯一落点 `capacity.LoadForCreate`。

**F9 计数落盘策略（2026-10-12 落地，用户裁定）**：**增加（`TryAdd`）恒同步落盘**（崩溃丢增加 =
used 偏低 = 限额放宽，不可接受）；**减少（`Release`）只标脏**，由 `Flush` / 待写次数阈值
（`counterFlushEveryOps=64`）/ 下次增加 触发——崩溃最多丢减少 → used 偏高 → **fail-closed（安全方向）**。
`CapacityFS` 不再每次写/删后落盘；新增 `Counter.Flush`，并在**关机**（`Handlers.flushVolumeCounters`
→ `volSet.Close` 前）与**摘除用户卷**前 flush（不 flush 会让每次重启 used 偏高、逐次累积成伪 507）。

**未纳入本次**（需产品/设计口径，另片）：**F1** 外部卷对象无用户可见删除/定位入口（写满后覆盖写
缩小也被全量探测拦下 —— 净增差量探测已在 #755 落地，删除/stat 入口属新面）。

**F1 外部卷回收入口（2026-10-12 落地，用户裁定「补 /delete 硬删 + /stat」）**：见 §8.12。

## 8.12 外部卷对象的硬删入口（2026-10-12 落地，评审 F1，P1 功能死路）

**问题**：外部卷没有 `*storage.Root`，`locateForRead`/`volumeFileExists`（须 `vs.Root(vol)`）恒
未命中 ⇒ 其对象**没有任何用户可见的删除/定位入口**。配置卷 `vol_capacity` 或用户卷 `capacity`
一旦被占满（`used == capacity`），该卷经文件 API 的**一切写入恒 507**（含「覆盖写缩小」——净增差量
探测已在 #755 落地仍受 `used` 高位阻挡），只能靠镜像/双向同步的删除传播或**运维手工删计数文件 +
重启**——功能性死路。

**处置（用户裁定：补 `/delete` **硬删** + `/stat`）**：

- **接缝**（`pkg/files`）：新增 `ExternalVolumeOps.ExternalDelete(ctx, owner, volName, remotePath,
  expectedChecksum)` + `WithExternalOps` 注入（未装配 → 跳过外部卷分支，零回归）。领域层不感知卷/
  键空间/装饰器细节。
- **装配实现**（`pkg/server/files_service.go` 的 `externalVolumeOps`）：经**同一可信链**
  `Guard(Wrap(be.FS()))`（`be.FS()` 已含 `CapacityFS`）执行 Delete——链上 `TrustedVolumeFS.Delete`
  完成「删主文件 + 联动删 sidecar」，`CapacityFS.Delete` 完成「**释放卷级计数**」；`storage.ErrSealed`
  → 409（封装卷禁止直连）。
- **定位语义**：显式 `?volume=` 指向外部卷 → 该分支终结（命中删、未命中 404 / `AllowMissing` 幂等）；
  未指定卷 → 遍历 owner 视图内的外部卷，**删到才终结**，都未删到则回落本地流程（默认卷可能仍有该
  文件，不能武断 404）。「外部卷」判据 = 无本地 Root（与 `volumeFileExists` 同源，非类型名硬编码）。
- **checksum 口径**：`expectedChecksum` 非空 → 与 sidecar **记录**的整文件 SHA-256 交叉比对（**不
  重算**——远端整文件回读不可接受），不一致 → 409；sidecar 缺失/不可读 → 跳过（无记录值可比）。
  **不做 quarantine**（远端后端无原子 rename）、**不进回收站**（硬删）。
- `/stat` 经读路径（`resolveDownloadPath` 的外部卷分支）**本就可用**，本次补了回归用例钉住。

**同时修掉一个 #765 归口引入的回归**：S3 `DeleteObject` **无 checksum 语义**（客户端不发
`X-File-Checksum`），而 `DeleteFile` 的 checksum 门禁先回 400 ⇒ **S3 DELETE 既有对象恒 400
「缺少 X-File-Checksum 请求头」**（既有测试只覆盖「删不存在对象」，故 CI 未捕获）。处置：新增
`DeleteFileInput.SkipChecksumVerify`（协议面显式跳过；安全由同 rel 文件锁 + 本地卷 quarantine 承担），
S3 DELETE 置位；`verifyDeleteQuarantine` 对空 `expectedChecksum` 只算不比对（S3 既有对象可删）。

**测试与变异**：`pkg/server/delete_external_test.go`（硬删 + 卷级计数释放 + checksum 不一致 409 且
对象保留 + sidecar 联动删除 + 显式卷未命中 404/幂等 + HTTP 面 `POST /delete` + 外部卷 `/stat` +
**S3 PUT→DELETE 无 checksum 成功**）。**变异 4 命中**（去外部卷分支 / S3 不跳过 checksum /
quarantine 强制比对 / 去 checksum 交叉比对）。

## 8.9 崩溃窗口（陈旧 sidecar）处置（2026-10-12 落地）

**问题**：主文件与 `.meta` sidecar 是**两次独立原子写**，崩溃落在「主文件已落新内容、sidecar
未更新」之间时留下**陈旧 sidecar**（`FileMeta.Size` ≠ 实测内容长度）。此前该文件表现为
**永久 fail-closed 且无提示**（流中首读才报「长度不一致」，运维只看到下载中断）。

**处置（不改变 C2 安全口径）**：
- **仍在流下发之前**判定（`filesMetaPolicy.VerifyDownload`，`seekEndSize` 探测），并**继续拒绝
  下发未校验内容**——不静默降级为「直通/未校验」，否则「内容被截断/追加」会被洗白成可信态；
- 归类哨兵 **`meta.ErrStaleMeta`**（`errors.Is` 可判），把「陈旧」与「sidecar 损坏（解析失败）」
  分开，供 API/告警分类；
- 专用 **Warn**（`warnStaleMeta`：`path`/`content_bytes`/`meta_bytes`/`cause`）并给出**可操作修复**：
  重建 sidecar = 重写该文件（任何写路径都会重算）或调用 `filesMetaPolicy.WriteMeta`；
- **不做自动重建**：自动重建会把「攻击者截断后的内容」重新认证为可信态（洗白通道），
  故重建必须由显式写路径/运维动作触发。

**测试**：`pkg/server/files_meta_stale_test.go`（正常校验通过 → 崩溃模拟 → `ErrStaleMeta` + 告警
→ 重建后恢复可校验读 → 截断同样归类）；变异验证 2 命中（去预检 / 去专用告警）。

**附：meta tmp 孤儿清扫节流（2026-10-12，第 5 轮评审 ⑥）**：`sweepMetaTmp` 原在每次 meta 写时
`ReadDir` 整个 meta 目录——扁平大目录下 n 个文件各写一次 = **O(n²)** 枚举。现改为**进程级节流**
（`metaTmpSweepThrottle`，默认 1 分钟）：孤儿是惰性残留（不阻断任何读写、仅占少量 meta 桶配额），
多存活 ≤1 个窗口属可接受降级；解析失败路径与写路径共用同一节流。

**附：server 不再直操作底层 root 的用户内容面（2026-10-12，第 5 轮评审「机制级收敛」）**：
`pkg/server/user_visible_io.go`（`writeUserVisible`/`openUserVisibleRead`/`userVisibleSize`）是用户内容
读写的**唯一入口**（加密卷透明加解密 + 封装封印）。此前 S3 GET 非加密分支、归档打包、checksum、
卷导出、分享创建探测、版本恢复写、云归档输出、`computeMeta` 均直调 `root.Open/OpenFile`——加密卷上
**读密文/写明文**。已全部收敛：
- 读面（archive.go/checksum.go/volume_export.go/share.go/s3_server.go/computeMeta）→ `openUserVisibleRead`
  （加密卷 `OpenDecrypted` 明文）；
- 写面（version.go 恢复 / cloud_archive_handler.go 归档输出）→ `writeUserVisible`；
- `encReadCloser` 补 `Stat`（明文长度 FileInfo）、`encWriteCloser` 补 `Sync`——使归档 TOCTOU 交叉
  验证、tar header 组装、fsync 在加密卷上仍可用；
- **加密卷 TOCTOU 已知边界**：`os.SameFile` 对 Lstat 与 OpenDecrypted（内部两次底层 open）在 Windows
  恒 false → 归档交叉验证降级为「Mode 一致」弱判据（非加密卷仍严格 SameFile），记档；
- 归档 tar header 大小用**用户可见长度**（Lstat 密文 size ≠ 明文流）；
- 剩余 `root.Open/OpenFile` 直操作仅限：meta sidecar、s3 part/会话、删除/目录类（不读内容）与
  `user_visible_io.go` 自身——内部面，不属用户内容读写入口。

**附：用户可见入口门禁 R25（2026-10-12）**：`internal/archcheck/user_io_gate_test.go` 把
「用户内容读写必须经 `openUserVisibleRead`/`writeUserVisible`（加密卷透明加解密 + 封印）」从
注释纪律升级为**硬门禁**：全仓非测试源码中 `root.Open/OpenFile(rel)` 调用（用户内容面）仅允许
`user_visible_io.go` 自身与白名单（meta sidecar / s3 part·会话 / 块写·分块在途暂存 / openCiphertext
密文导出例外 / 分享探测），其余一律违规。门禁发现并驱动的**新收敛**：`version_store.copyVersionFile`
（版本保存源读/目标写）、`version.go` 恢复源读、`dedupFallbackCopy`、`verifyDeleteQuarantine`、
`dupScanFile`、`verifyFileWithChecksumRoot` —— 加密卷上版本/去重/查重/删除校验全部按**明文**语义。

## 8.13 备份源经 Root（加密卷/封印卷可用）+「空路径」语义一致性（2026-10-12）

三处同源缺陷（都是「空路径 = 卷根/整卷」在某一层被当成非法）：

1. **备份源此前是裸 `NewLocalFS(srcAbs)` ⇒ 加密卷源读到密文**，故代码对加密源显式 409 拒绝
   （"加密卷暂不支持作为备份源（待接入经 Root 的备份源）"）。处置：源改为
   `NewPrefixFS(NewRootFS(srcTnt.Root()), <user 桶>/)`——经 `storage.Root` 即自动获得
   **at-rest 透明解密**与**封印**语义（键空间与目标一致：源条目仍是裸 rel，前缀映射到
   `user/<rel>`）；**加密卷与封印卷均可作为备份源**，且产物是**明文**（可还原）。
2. **`sync.FS` 契约里空串 = 卷根**（`LocalFS.SanitizeRelPath("")` 返回 ""、
   `WalkEntries(ctx, fs, "", …)`/`backup.Run` 均以 "" 起步），而 `pkg/server/root_fs.go` 把 ""
   直接交给底座 `os.Root`，后者 `Open("")`/`Stat("")` 以 `openat : empty path` 失败 ⇒
   经该适配器「列/stat 卷根」恒失败（任何从 "" 起步的遍历——含新备份源——都不可用）。
   处置：`rootRel("") → "."`（只映射空串；`.` 保持），子条目 Path 仍为裸相对路径（与
   `LocalFS` 一致）。
3. **`storage.SetSeals` 把空路径判非法**（"封印路径不能为空"），而 `enclosure` 与
   `docs/config.md` 明确「`extra.source` 子路径可空 = 整卷封装」⇒ 该配置装配期恒失败
   （boot fail；两侧各有测试故长期未暴露）。处置：**空字符串 = 整卷封印**（`sealCovers` 特判
   覆盖任意路径；纯空白仍拒绝 = 拼写错误）；整卷封印的运维语义见 `docs/config.md`
   （覆盖服务端内部桶 ⇒ 只应指向专用卷）。

**测试与变异**：`pkg/storage`（整卷封印 hidden/readable + `SealAt` 覆盖面 + 纯空白仍非法）、
`pkg/server`（`TestRootFS_EmptyRelIsVolumeRoot` 含 `WalkEntries("")` 集成点；
`TestApplyEnclosureSeals_WholeVolumeSealsEntireSource` 装配接线；
`TestBackupAPI_EncryptedSourceBacksUpPlaintext` 加密源 → 产物明文）。**变异 5 命中**
（空路径判非法 / 空规则不覆盖 / rootFS 不映射 / 备份源回退裸 LocalFS / 保留 409）。


## 8.14 回收站清理按「用户可见逻辑长度」释放配额（2026-10-12）

**缺陷（评审勘出）**：`package files` 的 `EmptyTrash`/`CleanupTrash` 用 `os.Stat(条目).Size()`
（**磁盘字节**）释放 owner 配额，而 at-rest 加密卷上磁盘是**密文**（容器头 + 每分块 tag/padding
膨胀），写路径预留与软删保留的都是**逻辑长度** ⇒ 释放量失真：密文 > 逻辑时**多释放**（owner 配额
虚低 → 超限写入通过），反之留幽灵占用（假 507）。痕迹：`releaseTrashEntryQuota(owner, root,
entryName, size)` 收 `root` 参数却**从未使用**——「本该用 Root 取逻辑长度」的残留。

**处置**：`releaseTrashEntryQuota` 去掉 `size` 参数，内部经 `trashEntryReleaseSize(root, entry)`
用 `Root.LogicalSize`（与写路径同口径）取释放量；取不到 → **返回 0 = 宁少放不多放**（释放不足
偏向「配额偏高」，由对账自愈；过度释放会让超限写入通过，属不安全方向）。两个调用点
（`EmptyTrash` 递归清理、`CleanupTrash` TTL 过期）同步收敛，`os.Stat` 取 size 的裸 I/O 一并消失。

**测试与变异**：`pkg/files/trash_test.go` 的
`TestTrash_EmptyEncryptedVolume_ReleasesLogicalSize`（加密卷写 5000+1000 → 软删 5000 → 清空回收站
→ 配额必须回到剩余文件逻辑长度 1000；并断言**前置条件**「密文 > 逻辑」，否则用例不具区分力；
末尾再走一遍 TTL 路径 `CleanupTrash`）。修复前实测 **948（少 52 字节 = 密文膨胀）**。
**变异 2 命中**（回退磁盘字节 / 恒不释放）。

**同族自查（本次一并核对，无同类缺陷）**：删除路径 `verifyDeleteQuarantine` 经
`root.OpenDecrypted(...).Stat()`（**解密流**的长度 = 逻辑长度）✓；分块上传覆盖写释放用会话内
`prev`/`total`（明文口径）✓；sidecar 写面用 `len(data)`（明文 JSON 长度）✓。

## 8.15 卷根直连门禁（R26）与「跨层约定不一致」审计（2026-10-13）

**门禁（机制化）**：新增 `internal/archcheck/volume_root_bypass_gate_test.go`（R26）——非测试
源码中 `sync.NewLocalFS(...)` 的调用点必须登记并写明「为什么可以不经 Root」。立项理由：历史三个
P0（DAV 面绕加解密/封印、备份源读密文、同步本地侧落明文）全部出自该类调用，且都靠逐次评审才
发现（单层测试发现不了：两侧各有测试，跨层才矛盾）；设计 §12 早已承诺该门禁但从未落地。判定用
AST（`go/parser`）而非正则：注释/字符串里的同名文本不误报，`NewLocalFS (` 带空格写法也不漏报；
并带**白名单空转自检**（登记文件已无调用即报红——门禁静默失效比报红更危险）。变异验证：未登记
调用 / 带空格写法 / 去掉登记 / 白名单空转 → 红；纯注释与字符串提及 → 绿。

**同批审计（结论含负结论）**：
- **配置文档 vs 解析**：`docs/config.md` 的 184 个配置键在 Go 代码中全部有引用，且**无
  comment-only** ⇒ 该面干净（两轮筛：先「文档有代码无」，再排除只在注释中出现者）。
- **`NewLocalFS` 站点逐处复核**：`enclosure/attach.go`（特权源视图，设计内）、
  `secret_register.go`（secrets 卷/secretdata 独立 target）、`pikpak_secretstore.go`（独立密钥
  目录）、`syncexec/executor.go`（未注入提供者时的回落）。**未修待裁定**：默认 secrets 卷落在
  `<storage_root>/secrets`（卷根子目录）——在 `extra.encrypt` 的默认卷上既不加密也不受封印；
  改经 Root 需先定**迁移口径**（既有明文 secrets 文件在加密卷上无法直接 `OpenDecrypted`，禁
  静默降级）。
- **裸 `os.*` 文件 I/O 面复核**：`volume_retention.go` 的 `os.ReadDir(<卷根>/<owner>/version)`
  仅为目录列举（目录名不加密）✓；`share.go`/`search_index.go` 落在服务端内部目录 ✓；
  `trash.go` 的 size 口径缺陷见 §8.14（已修）。

## 8.16 卷内 secrets/ 目录经 Root（加密卷上密钥材料密文落盘）+ pikpak 主密钥同口径（2026-10-13）

**缺陷（R26 登记项的「待裁定」闭环）**：默认 secrets 卷落 `<storage_root>/secrets` —— 即**默认卷
根之下**，而装配用的是裸 `sync.NewLocalFS`（纯 os I/O）⇒ 当默认卷启用 `extra.encrypt` 时，
**卷内容全加密而密钥材料明文落盘**，at-rest 加密对该配置形同虚设（拿到磁盘即拿到密钥）。
同类：pikpak 账号凭据的**主密钥**（`<storage_root>/secrets/pikpak-master`）由另一路径以裸 FS 写。

**处置**：
- `pkg/server/secrets_fs.go`：新增 `rootSecretsFS`（叶子适配器，经 `storage.Root`）——
  `OpenRead` 解密读、`WriteFile`/`ListDir`/`Stat`/`Delete`/`MakeDir`/`Rename` 全经 Root
  （封印随之生效；`ListDir`/`Stat` 用 `LogicalSize` 报用户可见长度）；`ChmodSecret` 承接本地
  0600 加固（经 Root 的视图不是 `*LocalFS`，`secrets.Manager` 的类型断言会静默跳过 ⇒ 新增
  `secrets.WithLocalChmod` 回调）。
  **无兼容负担**（用户裁定 2026-10-13：尚未上线）：不做「历史明文识别 / 读时迁移」，secrets 一律
  加密形态；非加密容器按存储层既有报错 fail-closed（不静默降级、不自动改写）。
- `cmd/sproxy` 装配：`secretsFSFor(set, absDir, …)` **按路径归属**判定——目标目录落在某个已装配
  卷根内 → 经 Root；独立目录 → 保持裸 `NewLocalFS`（R26 已登记）。目录布局恒为
  `<root>/secrets/<名>`（父目录 + `secrets` 段，两个入口一致）。默认 secrets 卷与
  `type: secrets` 卷两处入口共用该判据。
- pikpak：`pikpakEncryptedSecretStore` 增加卷根参数，主密钥经 `server.NewRootSecretsFS` 写入
  （**与默认 secrets 卷同口径**——两个写入者形态不一致会让对方把密文当密钥读，属永久性损坏）；
  服务器路径经 `registerPikpakDownloader(cfg, set)` 传入，CLI 路径按配置用新导出的
  `server.OpenLocalVolumeRoot` 打开同一卷根。凭据本体仍是 `secretdata` 容器（已由 shardseal 加密）。

**测试与变异**：`pkg/server`（密文落盘 / 读回明文 / 封印拒写 / 0600 加固）、`pkg/volume/secrets`
（`WithLocalChmod` 回调被调用——平台无关，Windows 上也守得住）、`cmd/sproxy`（加密默认卷下 secret
密文落盘 + 权限 0600（非 Windows）；pikpak 主密钥密文 + 复用可读；`<root>/secrets/<名>` 布局 +
封印生效）。**变异 7 命中**（secrets 写不经 Root / 装配回退裸 LocalFS / chmod 回调失效 /
pikpak 主密钥回落裸 FS / 目录漏拼 `secrets` 段（该缺陷由 e2e_cli_secretdata 实测暴露：secretdata 卷
`secret_url` 解析失败 → boot fail）等）。**e2e 回归**：`test/` 的 secretdata/secrets 真二进制
套件（装配 + 跨包互操作 + 多 target 副本 + 播放上传/云转存到 secretdata 卷）本地全绿。

## 8.17 搜索索引快照经 Root（加密卷上文件名密文落盘 + 封印生效）（2026-10-13）

**缺陷（跨层约定不一致：文档承诺 vs 实现）**：`pkg/files/index_persist.go` 的搜索索引快照
（`<tenant>/meta/index/<owner>.json`）用**裸 os I/O + 绝对路径**读写 ⇒ ① at-rest 加密卷上
快照**明文落盘**，而快照条目含**用户可见文件名**（`name`/`base`/`rel`）——`docs/config.md` 的
「落盘字节经 AES-256-GCM 分块加密」对该配置落空；② **封印被绕过**（整卷封印下仍能写入被封装区间）。
（时序索引本身无泄漏：索引构建走 `Root.ReadDir` ⇒ 封印条目已被隐藏过滤。）

**处置**：快照统一**经 `storage.Root`**：写用 `OpenFileEncrypted`（非加密卷 = 普通写，加密卷透明
加密）+ `AtomicRename` 原子替换；读用 `OpenDecrypted`（历史上的明文快照在加密卷上解密失败 →
视为「无快照」回退**全量重建**——**无兼容迁移**，用户裁定尚未上线，且快照数据可完全再生）；
失效删除同经 Root（封印路径恒拒，删除失败仅忽略）。读取加 8MiB 上界（防注入超大 JSON 的 OOM，
超限即视为损坏 → 重建）。**副作用（正向）**：封印区间写入现在 fail-closed（`ErrSealed`），
不再有「绕过封印往被封装区间写元数据」的路径。

**测试与变异**：`pkg/files`（加密卷：递归扫描磁盘无明文文件名 + 载入还原条目；封印区间写入
`ErrSealed`）。**变异 3 命中**（写回退裸 os → 文件名明文；读回退非解密读 → 载入失败；
写用非加密 `OpenFile` → 明文落盘）。文档：`docs/config.md` 的加密覆盖面写精确（内容 + 索引
快照加密；sidecar/tags 元数据明文，理由同列）。

## 8.18 用户标签（meta/tags）经 Root（加密卷上标签密文落盘 + 封印生效）（2026-10-13）

**缺陷**：`pkg/files/tags.go` 的标签 store（`meta/tags/<sha256(rel)>.json`，**标签的权威事实源**）
用**裸 os I/O + 绝对路径**（`tagsStoreAbs`）读写 ⇒ ① at-rest 加密卷上标签**明文落盘**（标签是
**用户数据**，与文件内容同敏感度）；② **封印被绕过**（整卷封装下仍能往被封装区间写标签）。
（同族：§8.17 搜索索引快照；本类根因统一为「绝对路径 + 裸 os」——见 §8.19 起的门禁。）

**处置**：删除 `tagsStoreAbs`，读写全部**经 `storage.Root`**：写用 `OpenFileEncrypted`（非加密卷
= 普通写）+ `AtomicRename` 原子替换；读用 `OpenDecrypted` + 读取上界 `maxTagsFileBytes=1MiB`
（超限视为损坏 → 与「未打标」同语义）；清空标签 = `Root.Remove`（`ErrNotExist` 幂等）；
子树清理/重命名搬移同经 Root。**副作用（正向）**：封印区间写标签现在 fail-closed（`ErrSealed`）。

**测试与变异**：`pkg/files/tags_test.go`（加密卷：递归扫描磁盘无明文标签 + 载入还原 + 清空幂等；
封印区间写入 `ErrSealed`）。**变异 3 命中**（写回退裸 os → 标签明文；读回退非解密读 → 载入为空；
写用非加密 `OpenFile` → 明文落盘）。

## 8.19 云任务状态 / 分享 / 计量台账经 Root（同族收口）（2026-10-13）

**缺陷（与 §8.17/§8.18 同族：绝对路径 + 裸 os I/O 绕过 `storage.Root`）**：

| 台账 | 位置 | 敏感性 |
|---|---|---|
| 云任务状态 JSON | `<租户根>/meta/cloud/<taskID>.json`、`groups/<groupID>.json` | URL / 文件名 / 目标路径（用户可见元数据） |
| 分享链接台账 | `<anonymous 租户根>/meta/share/<token>.json` | **分享 token 凭据** + 目标 rel |
| 计量台账 | `<默认卷根>/meta/usage/<owner>/<YYYY-MM>.json` | 用户用量画像 |

三者在加密卷上**明文落盘**（加密承诺落空），且都**绕过封印**（整卷封装下仍能写入被封装区间）。

**处置（统一形状：使用方不理解加密态，交给对应卷）**：全部改为「**卷句柄 + 卷内相对目录**」：
写用 `writeUserVisible`（`pkg/server` 的统一入口，内部按卷加密态分派）/ `root.OpenFileEncrypted`
（`pkg/cloud` 域包侧），读用 `openUserVisibleRead` / `root.OpenDecrypted`；目录创建 `ensureDir`
（`rel == "."` = 卷根，no-op）、原子替换 `root.AtomicRename`。
`PersistDirFor` 降级为**仅供布局断言/排查**（并删除已无使用者的 `groupsDirPath`/`groupsDirFor`）；
`usagePersistDir()` 收敛为 `usageRel` 常量 + 默认卷句柄；分享的 StateStore 回退读（legacy meta）
同样经卷句柄（否则加密卷上旧记录解析失败）。新增读取上界（状态 8MiB / 分享 64KiB / 月快照 4MiB）
防注入超大 JSON 的 OOM。

**测试与变异**：`pkg/cloud/persist_root_test.go`（加密卷：状态密文落盘 + 重启恢复；封印写入
`ErrSealed`）、`pkg/server/meta_ledger_root_test.go`（分享/计量：加密卷密文落盘 + 重启恢复；
封印区间 fail-closed）。**变异 6 命中**（状态写/读回退裸 os、分享写回退裸 os、计量写/读回退
裸 os、写用非加密 `OpenFile`）。

## 8.20 校验和台账（meta/checksums.json）经 Root（同族最后一处）（2026-10-13）

**缺陷（同族：路径式构造 + 裸 os I/O 绕过 `storage.Root`）**：`pkg/checksum` 的
`ChecksumStore` 以**绝对路径**构造（`<租户根>/meta/checksums.json`）并全程裸 os 读写。
台账**键是用户可见相对路径**（`user/<名>` → sha256），故在 at-rest 加密卷上：
① 明文落盘（加密承诺落空）；② 绕过封印（整卷封装下仍能写入被封装区间）。
`StateBackedChecksumStore`（集群模式）的**旧 meta 回退读**同样是裸 `os.ReadFile`：
加密卷上读到的是密文 → 解析失败 → 迁移期台账静默丢失。

**处置**：构造入口改为**卷句柄 + 卷内相对路径**（`NewChecksumStore(root, rel, logger)`；
生产 `rel = "meta/checksums.json"`，常量 `checksumRel` 在 `pkg/server`）：写
`root.OpenFileEncrypted` + `root.AtomicRename`、读 `root.OpenDecrypted` + 上界
`maxChecksumStoreBytes=64MiB`（超出视为损坏 → 空台账 + Warn，与「不存在/解析失败」同语义）；
tmp 残留清理改 `root.Remove`。`NewStateBackedChecksumStore(st, key, legacyRoot, legacyRel, logger)`
的旧 meta 回退读同样经卷句柄。**顺带**去掉历史上的「重命名失败就直写目标」回退——
`storage.Root.AtomicRename` 自带短退避重试，失败即真实失败（不再有半写窗口）。
非加密卷行为/字节/路径**完全不变**（判据在 Root：`if rt.enc == nil { return f }`）。
调用点适配：`pkg/server`（handlers + 6 处测试）、`pkg/files`（2）、`pkg/cloud`（1）、
`pkg/storage/capacity`（2）、`pkg/checksum`（5）——测试统一经 `testRootAt`/`openTestRoot`
拿卷句柄。

**测试与变异**：`pkg/checksum/store_root_test.go`（加密卷：台账密文容器 + 不含台账键、
重载/删除往返；封印区间 `save()` 返回 `ErrSealed`；适配器旧 meta 回退读在加密卷上可读回）。
**变异 4 命中**（写回退裸 os、读用非解密 `OpenFile`、写用非加密 `OpenFile`、适配器回退读裸 os）。
