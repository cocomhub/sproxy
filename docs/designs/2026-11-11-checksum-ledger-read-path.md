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
| **S1 口径对齐** | `fileInfosFromViewEntries` 优先 `e.Checksum`；抽 helper 统一 list/search 取值 | 无 | `TestList_ViewChecksumPreferredOverLedger`（构造 `e.Checksum` 与台账不同值 ⇒ 取视图值；变异：仍先查台账 ⇒ 红） |
| **S2 本地卷自报** | `volumestate` 增注入式 `metaReader`（opts）；`CacheFS.ListDir` 回源后补 checksum 并缓存；装配层经 `*storage.Root` + `files/meta` 提供 reader | S1 | 本地卷 list 的 checksum 不依赖台账（清空台账后 list 仍正确）；缓存命中不重复读 sidecar（计数注入）；变异：不做补全 ⇒ 红 |
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
