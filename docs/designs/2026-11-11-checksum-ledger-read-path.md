<!--

SPDX-License-Identifier: Apache-2.0
-->

# checksum 台账只读化：读路径口径裁定（2026-11-11）

> 决策文档。对应 `docs/designs/2026-10-28-volume-state-maintenance.md` §15.1「台账只读化」遗留项。
> §15.1.3 曾判「暂缓：在明确读路径口径前保持『写台账 + 按卷读』」；本文**给出该口径的裁定**并拆出可独立交付的切片。
> 基线：`62d118059`（master）。

## 1. 目标与非目标

- **目标**：让读路径**不再依赖 `checksums.json` 台账**，台账退化为导入源并最终删除写路径（R46 收敛至 0）。
- **非目标**：本次不改变写路径（仍写台账，保证过渡期零回归）；不做 `searchIndex` 整体替换（那是 §15.2，独立议题）。

## 2. 现状事实（代码级）

1. **卷自报 checksum 机制已存在**：`sync.Entry` 早有 `Checksum` / `ChecksumType` / `Checksums`（`pkg/sync/entry.go:23`）。
   - secretdata 在 `ListDir` 填 `ent.Checksum = e.meta.Original.SHA256` + `ChecksumType="sha256"`（`pkg/volume/secretdata/secretdata.go:840`、`:964`）；
   - 装配层 `ListDirView` 把它透传为 `ExternalEntry.Checksum`（`pkg/server/files_service.go:134-135`，仅 `sha256`）。
2. **搜索路径已用卷自报**：`collectViewHit` 直接 `Checksum: e.Checksum`（`pkg/files/search_external.go:140`）。
3. **❌ 列表路径丢弃卷自报**：`fileInfosFromViewEntries` 只查台账 `fi.Checksum = csAt(relName)`（`pkg/files/read_ops.go:282`），**未读 `e.Checksum`**。
   ⇒ 同一份数据，`/api/files` 与搜索的 checksum 口径不一致；secretdata/S3 的自报 checksum 在 **list** 上被浪费（`ListDirView` 注释宣称「透传」在 list 路径未生效）。
4. **本地卷 FS 链没有 meta 读取能力**：`meta.Provider` 的实现者只有装饰器（`PrefixFS`/`StagingQuotaGateFS`/`CapacityFS`/`SealFS`/`ownerFS`/`TrustedVolumeFS`/`MetaBucketGuard`/secretdata/cluster）与 `volumestate.CacheFS`（委托 inner）；**本地卷核心 `rootfs` 不实现**。本地卷的 meta sidecar 由 `pkg/files` 经 `*storage.Root` 读写（R28 已登记）。
5. **唯一列举入口的缓存层已具备所需条件**：`pkg/volumestate` 已 import `pkg/files/meta` 并有 `CacheFS.FileMeta` 委托（`pkg/volumestate/wrap.go:266`）；`CacheFS.ListDir` 缓存 `[]syncpkg.Entry`（`wrap.go:45`）。
6. **R46 真实可降点只有 5 个**（`pkg/server/handlers.go` ×3、`pkg/files/search_index.go:839`、`pkg/files/service.go:657`），其余 24 点是注释/文件名字面量噪音、6 点永久保留（meta 桶跳过规则）。

## 3. 候选读路径口径

| 方案 | 做法 | 结论 |
| --- | --- | --- |
| **A. 卷自报 checksum** | 列举时由卷/装饰链填 `sync.Entry.Checksum`，消费方直接取 | **采用（渐进式）**；机制已在（secretdata），缺口在「本地卷」与「list 用了没」 |
| **B. 台账加卷维度** | 键 `<vol>\x00<rel>` | 已在 §15.1.2 落地（修正确性），但与本目标（脱离台账）方向相反 |
| **C. list 逐文件读 sidecar** | 每次 list 现读 meta | 弃：大目录 N 次 I/O（台账正是为避开它而存在） |

## 4. 裁定（推荐 A 的落地形态）

**读口径 = 「视图条目自报 checksum」为第一优先，「台账」为过渡期兜底；本地卷的自报由列举缓存层补齐。**

具体：

1. **消费方（list）与 search 对齐**：`fileInfosFromViewEntries` 改为
   `if e.Checksum != "" { fi.Checksum = e.Checksum } else { fi.Checksum = csAt(relName) }`。
   - 收益：立即消除 list/search 口径漂移；secretdata/S3 的 list checksum 不再白算。
2. **本地卷自报的落点 = `pkg/volumestate.CacheFS`（唯一列举入口的装饰器），而非 `rootfs`**：
   - 在 `ListDir` **cache-miss 回源后**，对目录条目按注入的 `metaReader(vol, path) (sha256 string, ok bool)` 补 `Entry.Checksum/ChecksumType="sha256"`，**随快照缓存**（每目录每代际一次，与现有台账 map 查找同数量级，且被缓存吸收）。
   - `metaReader` 由**装配层注入**（装配层是唯一持有 `pkg/files` meta store 与 `*storage.Root` 的地方）⇒ 不引入「`volumestate` 反向依赖 files 领域」的架构倒置；`volumestate` 只依赖已 import 的 `meta` **类型**。
   - 为什么不是「让 rootfs 读 sidecar」：meta 的物理布局（`.meta` sidecar 路径、加密卷差异）是 `pkg/files` 的领域知识；下沉到卷层即 §15.1.3 指出的架构倒置。
3. **台账退化路径**：S2 落地后，读路径不再需要台账；`Set/Delete/Rename` 的台账写可删，`checksums.json` 退为一次性导入/运维自愈源（`sclient checksum rebuild`），最终删除 R46 门禁。

## 5. 分期切片（每片可独立 PR）

| 片 | 内容 | 前置 | 验证 |
| --- | --- | --- | --- |
| **S1 口径对齐** ✅ 已交付 | `fileInfosFromViewEntries` 优先 `e.Checksum`（未自报时回落台账）；与 search 路径同口径 | 无 | `TestList_ViewChecksumPreferredOverLedger`（构造 `e.Checksum` 与台账不同值 ⇒ 取视图值；变异：仍先查台账 ⇒ 红） |
| **S2 本地卷自报** ✅ 已交付 | `volumestate.CacheFS.ListDir` 在 **cache-miss 回源后**用 inner 的 `meta.Provider` 读 sidecar `TotalSHA256` 补齐条目 checksum（**仅本地卷**；已有自报值/目录条目跳过；sidecar 缺失留空）。**无需装配层改动、不新增 R28** | S1 | 单测 `TestCacheFS_ListDir{FillsLocalChecksums,ChecksumCached,SkipsRemoteChecksums,LocalChecksumPersisted}`（含**落盘快照带 checksum**）；变异 2/2 命中（去本地性判定 ⇒ 远端红；填充移到 `Get` 外 ⇒ 落盘用例红） |
| **S3 台账写路径窄化** | 读路径去掉台账兜底；`Set/Delete/Rename` 停写；`checksum_rebuild` 只保留显式导入 | S2 | 全量 list/stat/search 用例（含多卷同名、加密卷、外部卷）；R46 计数按其真实值下调 |
| **S4 收口** | 删 `checksums.json` 写路径 + R46 门禁与 TSV（0 行时无意义） | S3 | `go test ./internal/archcheck/` 全绿；回归套件全绿 |

**S3 的前置口径（本片已定）**：台账在过渡期仍写，只是为了「S1/S2 未覆盖的路径」（如版本桶，§15.1.3 已裁「不迁移」）不失真；S3 只停「user 内容」的台账写，版本桶键保留。

## 6. 风险与边界

- **远端卷**（S3/baidupcs）：每文件读 checksum = N 次往返 ⇒ 维持「不主动补」策略（当前 `ListDirView` 只在底层已自报时透传）。S2 的 `metaReader` 只对**本地/可信卷**注入，外部卷维持空 ⇒ 消费方按需 stat 兜底（既有语义不变）。
- **加密卷**：`Entry.Checksum` 必须是**明文** sha256（secretdata 已是明文 meta）；`rootfs` 加密卷经 `CacheFS` 时不得把密文哈希填入（S2 的 reader 必须走 `pkg/files` 明文 meta，而非卷内原始字节）。
- **缓存一致性**：checksum 随目录快照缓存；写路径已由 `CacheFS` 自动失效（P3），但**进程外**改动遵循既有「显式 refresh / 默认永不过期」契约（§15.3 决策 b）。
- **不做**：不为「本地卷 StateGeneration」等低收益项立项（§15.1.3 已裁）；不改 `searchIndex`（§15.2 独立）。

## 7. 与门禁的关系

- R46（`legacy_state_usage`）的下降应由 S1→S3 **真实消除**驱动，不通过改口径取巧（口径噪音的剔除另见 `docs/archive/gate-ratchet-reduction.md` 的 D3 决策）。
- 新增的注入式 `metaReader` 属**构造注入**（结构体私有字段 / opts），符合「测试注入纪律」（禁包级 seam 替换）。

## 8. S2 实施记录与**验收口径修正**（2026-11-12）

**实现**（比原计划更小，且更贴合现有架构）：
- 落点从「装配层注入 `metaReader`」收敛为 **`pkg/volumestate.CacheFS` 内部**：cache-miss 的
  `fetch` 内，用 `inner.(meta.Provider)` 读 sidecar（`trusted.Wrap` 已实现 `FileMeta`，且 R21 要求
  装饰器全量转发 ⇒ **本地卷 ViewUser 链本就有该能力**），把 `TotalSHA256` 填进
  `Entry.Checksum/ChecksumType/Checksums`。
- **仅本地卷**（`syncpkg.LocalVolume`）：远端卷每文件一次网络往返 = 性能回归。
- 填充在 **`fetch` 内** ⇒ 随目录快照缓存，**并随快照落盘**（重启后磁盘命中零 sidecar 读）。

**验收口径修正（重要）**：原 S2 验收「清空台账后 list 仍正确」**不具判别力**——台账为空时
现有自愈路径（§15.1 步骤 ①②：`RebuildChecksums` 从 sidecar 批量回填）已能让 list 给出正确
checksum；且台账是**内存优先 + debounce 落盘**，进程内删文件不改变内存态。⇒ S2 的可观测收益是
**「列表 checksum 直接来自 sidecar，无需台账/自愈」**（性能与单一事实源），其正确性由
`pkg/volumestate` 单测（含落盘快照用例）+ 变异锁定；API 级判别需测试专用 hook，暂不做。

**遗留（S3 前置）**：读路径仍保留台账兜底（`files.checksumGetter`）。S3 去掉兜底 + 窄化写路径前，
需确认「无 sidecar 的文件」（历史数据/旁路写）的读语义——届时按「现算 `fileChecksumRoot`」或
「显式 `--repair`」二选一，不得静默返回空。

## 9. S3 侦察结论：**暂不具备安全增量条件**（2026-11-12）

停写台账（S3）会同时影响两条**尚无替代来源**的读路径，必须先裁定语义：

| 读路径 | 现状来源 | 停写后的后果 | 需裁定 |
| --- | --- | --- | --- |
| 本地 **search** 结果 checksum | 台账（`pkg/files/search_index.go:724` 的 `cs(e.volume, e.name)`） | 搜索结果 checksum 变空（搜索不像 list 那样走卷视图/ S2） | 让 search 也走「视图/侧车」还是接受空值（由客户端 `stat` 兜底） |
| **远端卷**（pikpak/webdav/sftp/ftp）**列表** checksum | 台账（这些 FS 的 `ListDir` **不自报** sha256；`ListDirView` 只透传 sha256） | 远端列表 checksum 变空 | 远端是否接受「空 + 按需 `stat`」（会改变现有 UX） |

其余路径已有替代或可接受：
- 本地 **list** → S2（侧车，本 PR 已交付）；
- 本地 **stat** → 台账 miss 时**现算** `fileChecksumRoot`（`read_ops.go:461`，既有行为）；
- **s3/baidupcs** `ListDir` 自报（取各自原生哈希；`ListDirView` 只透传 `sha256`，需确认其类型是否为 sha256）。

**结论**：S3 不是「窄化写路径」的小改，而是要求「本地 search 与 list 统一数据源」+「远端卷列表 checksum 语义裁定」
两项前置；在裁定前**保持写台账**（当前无正确性缺陷）。S4（删门禁）随之推迟。

## 10. 候选方案（用户提议 2026-11-12）：**meta 单源 + 目录 meta**

> 用户设想：「meta 文件里直接存 sha256」+「目录也新增 meta 文件，简化管理」。

### 10.1 事实澄清（先对齐现状）

- 文件 sidecar **已经**存了 `TotalSHA256`（`pkg/files/meta.FileMeta.TotalSHA256`，`meta/<rel>.meta`）——
  「在 meta 里存 sha256」已于 §15.1 落地；台账的剩余角色**不是存储**，而是**聚合/索引**（list/search 的
  O(1) 查找面）。
- 目录级 meta **已有先例**，但语义不同：`pkg/volume/secretdata` 的 `shardseal.KindDirMeta` 是**逻辑树**
  （目录改名/移动的权威记录，带 `Version→mtime→Name` 仲裁与 `CleanupStaleDirMeta` 重试执行器）；
  **不是** checksum 索引。新方案应另起命名（如 `dirindex`），避免与它撞概念。

### 10.2 方案（D）：为**本地/可信卷**增加「目录 checksum 索引」

每目录一个聚合 meta（`<owner>/meta/<dir>.dirindex`，随目录失效），内容：
`{version, entries: {name: {sha256, size, mtime, type}}}`（可分片/上限）。写路径在
`trusted.Wrap` 的 `WriteFile`/`Rename`/`Delete`/`MakeDir` 之后**同步更新父目录索引**；读路径 list
**一次读一个文件**即得全目录 checksum。

| 维度 | 现状（S1+S2） | 方案 D（目录索引） |
| --- | --- | --- |
| list checksum 来源 | 条目自报 → sidecar 逐文件读（S2，缓存吸收）→ 台账 | **单文件读/目录** → sidecar 逐文件（回退） |
| 台账角色 | 聚合缓存（**不可删**） | 仅存量导入源（**可删** ⇒ 解锁 S3/S4） |
| search checksum | 台账（`search_index.go:724`）——**S3 阻塞点①** | 同目录索引（**解锁**） |
| stat | 台账 miss ⇒ 现算 | 同目录索引（可选） |
| 远端卷（webdav/sftp/ftp/pikpak） | 台账（**S3 阻塞点②**） | **不适用**（远端无 sidecar 写侧）⇒ 仍需单独裁定 |
| 写放大 | 每文件 1 次 sidecar 写 | 每文件 1 次 sidecar + **1 次父目录索引写** |
| 并发 | 无共享热点 | **同目录写需串行化**（目录级锁/合并），否则丢更新 |

### 10.3 关键设计点（决定成败）

1. **一致性**：文件写入与其索引更新必须**同事务**：先写文件 + 其 sidecar，再更新父目录索引；
   索引更新失败 ⇒ 记事项（复用 `pkg/server/issues` 的「重试直到成功」闭环），**不得静默**。
2. **仲裁/版本**：沿用 secretdata 的精神——索引带 `version`（每次更新单调递增）+ 目录 `mtime` 兜底；
   读取时若索引与实际目录条目不符（多用例：崩溃/外部改动）⇒ **按条目校验**（size/mtime）并重建。
3. **并发**：目录索引读-改-写必须持**目录级锁**（跨进程需 `pkg/server/filelock`）；高并发目录可改为
   「日志 + 定期压实」而非每写一次全量重写。
4. **迁移/自愈**：存量无索引 ⇒ 首次列举**重建**（现有 `RebuildChecksums` 同型：逐文件 sidecar 聚合，
   只落一次盘）；索引缺失/损坏 ⇒ 回退 S2 的逐文件读，不报错。
5. **规模**：单目录条目极多时索引会大（需分片，如每 1k 条目一个 shard），并纳入 `volumestate` 的
   条目预算（现 `DefaultMaxTotalEntries=200k`）。
6. **能力形态**：作为**可选能力接口**（如 `DirIndexReader`/`DirIndexWriter`）放到 `pkg/sync`
   （与 `StorageInspector`/`DirMetaCleaner` 同族），由装饰器转发（R21 自动覆盖），消费方 `Assert*` 探测。

### 10.4 与已交付 S2 的关系与分期

- S2（逐文件 sidecar + 快照缓存）**是方案 D 的回退路径**，不废弃：D 命中即单读，未命中/不符即回退 S2。
- 建议分期：**D1** 索引格式 + 写路径钩子（含目录锁与失败上报）；**D2** 读路径（list 优先索引 → 回退 S2 → 回退台账）；
  **D3** 迁移/自愈（重建 + 校验）；**D4** 台账只读化（原 S3/S4）——此时**本地侧**台账可删；
  **远端卷列表 checksum 语义**仍需单独裁定（本方案不覆盖）。
- 结论：该提议**方向正确且能把「本地侧台账」彻底退场**，代价是「写放大 + 目录级并发/一致性」复杂度；
  建议按 D1–D4 推进，**先做 D1 的格式与写路径**并保持读路径不变（零行为回归）。

### 10.5 需要用户裁定

- 是否按 D1–D4 推进（新增写放大与目录锁）？或**保持现状**（S2 + 台账只读兜底），仅做只读收敛？
- 若推进：远端卷列表 checksum 是否接受「留空 + 客户端 `stat` 兜底」（解锁阻塞点②）？
