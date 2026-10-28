# 统一卷状态维护：单一缓存层 + 卷自管能力（设计）

> 状态：**设计（待实施）**。来源：2026-10-28 用户明示 ——
> 「能否设计完善可靠的所有卷状态维护方案，支持收敛 checksum、list、索引等信息维护，
> 避免不同卷多套实现（secretdata 等支持自行管理，但默认也有基于本地 staging 的缓存提高性能）」
>
> 前置：`docs/designs/2026-10-27-remote-derived-state-cache.md`（A/B/C/D 分类与四层机制、
> S1/S2 已落地）、`docs/designs/2026-10-28-unified-volume-model.md`（StorageVolume +
> `VolumeView` 唯一取卷入口）。本设计是二者的**收敛续篇**：把「派生状态缓存」从各卷各写
> 收敛为**唯一实现**。
>
> 触发案例：PR #874（旁路写不登记派生状态 ⇒ 转存产物 list 不可见 + checksum 空）。

---

## 一、现状取证（2026-10-28）

### 1.1 三套实现、三个真相源、三条失效路径

| 面 | 本地卷 | 外部卷（baidupcs/S3/webdav…) | secretdata |
|---|---|---|---|
| **list 数据源** | `pkg/files/searchIndex`（内存 map + JSON 快照） | **每请求直连远端 `ListDir`（无任何缓存）** | 自管内存索引（挂载时 `loadIndex`） |
| **checksum 来源** | per-tenant `checksums.json` 台账 | `Entry.Checksum`（S3/baidupcs 只给 etag） | 内存 meta（O(1) 已填 sha256） |
| **失效方式** | `RecordBypassWrite` / `IndexMove`（#874 补） | 卷内各自 `invalidateDlink` 等 | 仅显式 `POST /api/volumes/refresh` |
| **递归搜索** | `searchIndex.search`（全树） | 不支持（返回空） | 不支持 |

### 1.2 由此产生的缺陷族（已发生或必然发生）

| 缺陷 | 结构成因 |
|---|---|
| #874 转存产物 list 不可见 + checksum 空 | 旁路写绕过写内核 ⇒ 未登记 searchIndex + checksum 台账 |
| 外部卷 list 每次全量远端往返（大目录卡顿） | 无缓存层 |
| secretdata 外部改动后幽灵条目（直到重启/显式 refresh） | 失效路径只有「挂载一次」 |
| 同名重建命中旧 dlink（#853 前） | 卷内缓存的失效靠各实现自觉 |
| 同一语义三处实现（list/checksum/失效） | **没有单一收敛点** |

### 1.3 关键既有资产（本设计复用而非新造）

| 资产 | 位置 | 复用方式 |
|---|---|---|
| `sync.Entry.Checksum/Checksums/ChecksumType` | `pkg/sync/entry.go` | **卷自报 checksum 的唯一通道**（已存在，S2 已填 secretdata） |
| `sync.FS`（ListDir/Stat/OpenRead/Write/Delete/Rename） | `pkg/sync` | 卷侧唯一数据面契约 |
| 可选能力模式（`Footprinter`/`PhysBound`/`ReserveSpace`/`IndexRefresher`/`MetaExtraUpdater`） | `pkg/volume`、`pkg/sync` | 卷自管状态的声明方式 |
| 节点本地产物约定 `<storage_root>/meta/<域>/<卷>` | `pkg/server/volumes.go:350`（容量账本） | 缓存落点沿用同约定 |
| `POST /api/volumes/refresh`（`IndexRefresher`） | `pkg/server/volumes_api.go:1017` | 显式失效入口（已存在） |
| `LogicalKey`（对象身份） | `pkg/sync/logical_key.go` | 失效键的构成基础 |

---

## 二、目标（用户裁定的四条口径）

| 口径 | 裁定 |
|---|---|
| 权威归属 | **卷权威，缓存仅加速** —— A 类 checksum 永不由缓存充当最终依据 |
| 缓存落点 | **本地 staging 目录 + 进程内 LRU**（节点本地，不进卷/不进网盘） |
| 失效强度 | **写穿 + TTL + 代际**（A 类强一致；C 类有界过期） |
| 范围 | **含 searchIndex 迁移**（保留兼容期，逐卷切换，最终删旧实现） |

**非目标（明确划界）**：D 类聚合账本（配额/容量）不迁入本层——它已有独立对账体系
（`pkg/volume/capacity`），且准入语义（fail-closed 预占）与本层的「读视图加速」不同。

---

## 三、目标架构：四层

```
┌────────────────────────────────────────────────────────────────────────┐
│ L3 消费入口（唯一）：files.List / Search / Stat                        │
│     ↳ 只认 voluntate.Cache，不再直连 searchIndex 或 ExternalList       │
└───────────────────────────┬────────────────────────────────────────────┘
                            │ ListDir(vol, owner, rel) / Search(vol, owner, q)
┌───────────────────────────▼────────────────────────────────────────────┐
│ L2 统一缓存 pkg/volumestate（唯一实现）                                │
│   DirView  : (vol,owner,rel) → {entries, gen, fetchedAt}   ← list      │
│   TreeView : (vol,owner)     → 全树投影（由 DirView 增量拼装）← search  │
│   存储      : 进程内 LRU  →  本地 staging JSON（<root>/meta/state/…）   │
│   失效      : ① 写穿 Invalidate(ObjectRef) ② TTL ③ 代际校验           │
└───────────────────────────┬────────────────────────────────────────────┘
                            │ 回源（miss / 过期 / gen 变化）
┌───────────────────────────▼────────────────────────────────────────────┐
│ L1 卷侧契约（唯一）：sync.FS.ListDir/Stat（Entry 带 Checksum*）        │
│    + 可选 StateGeneration（廉价代际探测；secretdata/baidupcs/S3 可实现）│
└────────────────────────────────────────────────────────────────────────┘
```

### 3.1 L1 —— 卷侧契约（不新造接口）

**必需**（已存在）：`sync.FS`
- `ListDir(ctx, rel) ([]Entry, error)`：`Entry` 必须尽可能填 `Checksum`/`Checksums`/`ChecksumType`
  （契约沿用既有设计 §3.1：「零额外成本就必须填」；远端逐文件 RTT ⇒ 显式留空 = unknown）
- `Stat(ctx, rel) (*Entry, error)`：同上（单对象权威）

**可选**（新增，能力接口风格与 `IndexRefresher` 一致）：

```go
// pkg/volume（域模型包，不依赖 pkg/sync）
// StateGeneration 是卷的**廉价新鲜度探针**：返回 (owner, rel) 子树的代际号。
// 语义：同代际 ⇒ 该子树的「结构视图 + 内容派生值」自上次取用后未变。
// 实现口径（卷自选，必须廉价，不得全量遍历）：容器 mtime/条目数/自增计数器。
// 未实现 ⇒ 缓存层退化为 TTL-only（外部卷默认）或写穿-only（本地卷）。
type StateGeneration interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
    StateGen(ctx context.Context, owner, rel string) (uint64, error)
}
```

**关键约束**：卷**不得**自己实现 list 缓存（除 L2 外，全仓禁止 list 级缓存实现——见 §六门禁）。
卷只负责「如实、廉价地报告当前状态」，加速由 L2 统一承担。

### 3.2 L2 —— 统一缓存 `pkg/volumestate`（唯一实现）

#### 3.2.1 数据模型

```go
// ObjectRef 是失效信号的最小单元（沿用既有设计 §3.3 的 ObjectRef 提法）。
type ObjectRef struct {
    Volume string // 卷名（"" = 默认卷）
    Owner  string
    Rel    string // 卷内相对路径（正斜杠；**不含** owner 前缀，与 sync.FS 键空间一致）
}

// DirSnapshot 是一个目录的**直接子项**快照（list 的缓存单元）。
type DirSnapshot struct {
    Entries   []sync.Entry `json:"entries"`
    Gen       uint64       `json:"gen"`       // 回源时卷自报的代际（未实现 = 0）
    FetchedAt int64        `json:"fetched_at"` // UnixNano
    Volume    string       `json:"volume"`     // 归属校验（防串卷）
}

// ownerTree 是 search 的全树投影（由 DirSnapshot 拼装；条目不重复存 checksum 以外的内容）。
type ownerTree struct {
    Entries map[string]TreeEntry `json:"entries"` // key = 相对卷根的 rel
}
type TreeEntry struct {
    Base     string `json:"base"`
    Size     int64  `json:"size"`
    ModTime  int64  `json:"mtime"`
    IsDir    bool   `json:"is_dir"`
    Checksum string `json:"checksum,omitempty"`
    ChecksumType string `json:"checksum_type,omitempty"`
    Volume   string `json:"volume"`
}
```

#### 3.2.2 缓存 API（消费方唯一入口）

```go
// ListDir 返回目录直接子项（缓存命中 → 校验新鲜度 → 回源）。
func (c *Cache) ListDir(ctx context.Context, vol, owner, rel string) ([]sync.Entry, error)

// StatObject 返回单对象视图（A 类：命中亦校验新鲜度；卷未实现 gen 时对**本地卷**直接回源）。
func (c *Cache) StatObject(ctx context.Context, vol, owner, rel string) (sync.Entry, bool, error)

// Search 在 owner 视图内递归匹配（由 TreeView 提供；远端卷可按装配开关禁用）。
func (c *Cache) Search(ctx context.Context, vol, owner, query, tag string) ([]TreeEntry, error)

// Invalidate 是**唯一失效入口**：写/删/改名成功后调用（写穿）。
// 语义：删该 ref 的树条目 + 标记 ancestors 的 DirView 为 stale（不立即回源）。
func (c *Cache) Invalidate(ref ObjectRef) error

// Refresh 显式重建（对应既有 POST /api/volumes/refresh）。
func (c *Cache) Refresh(ctx context.Context, vol, owner string) error
```

#### 3.2.3 落点与形式（用户裁定：本地 staging + 内存 LRU）

```
<storage_root>/meta/state/<卷名>/<owner>/<dirRel>/entries.json   # DirSnapshot（原子写 tmp+rename）
<storage_root>/meta/state/<卷名>/<owner>/tree.json               # ownerTree（可选，聚合投影）
<storage_root>/meta/state/<卷名>/<owner>/.gen                    # 最近一次已知代际（可选）
```

- **节点本地**（与容量账本 `<root>/meta/quota/<卷>` 同约定、同运维习惯）；**不进卷、不进网盘**
- 进程内 LRU（默认 4096 目录项 / 128MiB 上限，可配）在 staging 之上
- **容量与 GC**：`state_cache_max_bytes`（默认 256MiB，0 = 不落盘只用内存）；超限按 LRU 淘汰；
  启动清理 `*.tmp` 残留；卷删除时整目录回收
- **保密卷**：缓存只存**元数据**（name/size/mtime/checksum），不含内容字节；对
  `volumePrivate()` 卷默认**只落内存不落盘**（`state_cache_persist_private: false`，反义命名
  遵循 AGENTS.md 18 条默认 false 纪律）

#### 3.2.4 新鲜度判定（三重）

```
命中路径：
  snapshot := lru.Get(key) ?? disk.Load(key)
  if snapshot == nil                    → 回源（miss）
  if now - snapshot.FetchedAt > ttl     → 回源（TTL）
  if 卷实现 StateGeneration:
        gen, err := vol.StateGen(ctx, owner, rel)
        if err != nil                 → **回源**（探测失败 fail-open 到真值，不用可能过期的缓存）
        if gen != snapshot.Gen        → 回源（代际变化）
  返回 snapshot.Entries
```

**TTL 取值**（配置 `volume_state_ttl`，默认按卷类型分档）：

| 卷类型 | 默认 TTL | 依据 |
|---|---|---|
| 本地卷（含 at-rest 加密） | **0（不按时间过期）** | 写穿覆盖本进程；外部进程改动由代际/显式 refresh 兜底 |
| 外部卷（baidupcs/S3/webdav/ftp/sftp/federated/cluster） | **30s** | C 类有界过期；避免大目录反复远端往返 |
| 自管卷（secretdata） | **30s + 代际** | 卷内写即时更新自身索引；跨进程改动由代际发现 |
| 禁用 | `volume_state_ttl: -1` | 每次回源（调试/强一致场景） |

#### 3.2.5 A 类强一致的兜底（永不依赖缓存齐全）

沿用既有设计 §3.2 的三段式，并**提升为门禁要求**：

```go
// 任何「需要 checksum 才安全」的写操作：
//   ① 调用方给了 → 用；
//   ② 没给 → StatObject/FileMeta 现取（**StatObject 内部对本地卷直接回源，不用缓存值**）；
//   ③ 仍拿不到 → 明确拒绝（fail-closed），绝不静默跳过校验。
```

实现要点：`StatObject` 对「checksum 为空的条目」**不得**返回 (entry, true) 就当作「已校验」——
调用方必须能区分 `Checksum == ""`（unknown）与「有值且匹配」。

### 3.3 L3 —— 消费入口（唯一）

| 消费方 | 迁移后 |
|---|---|
| `files.Service.List` | `cache.ListDir(vol, owner, rel)`（本地/外部/secretdata 同一条路径） |
| `files.Service.Search` | `cache.Search(...)`（TreeView；远端卷按 `volume_state_search_remote` 开关，默认 false） |
| `files.Service.Stat`（读面） | `cache.StatObject(...)` |
| `ExternalVolumeOps.ExternalList` | **退役**（其语义并入 L2；保留一个兼容期薄转发，迁移完成后删） |
| `searchIndex.list/search` | **退役**（见 §五 V3/V4） |
| `RecordBypassWrite` / `IndexMove`（#874） | 收敛为「写成功后 `cache.Invalidate(ref)`」，删除重复的 index+checksum 双写 |

**为何 checksum 台账（`checksums.json`）也并入本层**：它今天与 searchIndex 是两份独立派生
状态、两条失效路径（#874 的根因）。合并后 checksum 只存于 `DirSnapshot.Entries[].Checksum`
（+ `TreeEntry.Checksum`），失效只有一个入口。兼容期保留 `checksums.json` 只读回退（见 V3）。

### 3.4 L4 —— 门禁（防回归）

| 门禁 | 判据 | 理由 |
|---|---|---|
| **R40** | `pkg/files`、`pkg/server`、`pkg/volume/**` 的非测试源码**不得出现新的 list 级缓存**（类型名含 `Cache`/快照/索引 map 且提供 list 语义）——白名单仅 `pkg/volumestate` 与已有的 `pkg/volume/secretdata`（自管，登记理由） | 防止再造第四套 |
| **R41** | `files.List`/`Search` 的实现体不得直调 `index.list`/`ExternalList`（迁移完成后） | 保证单一入口 |
| **R42** | 新增 `sync.FS` 实现的 `Delete`/`Rename`/`WriteFile`，若其类型实现了 `StateGeneration` 或持有派生状态，必须同时提供失效（`Invalidate` 回调或自述「我有自管索引且已失效」注解） | 防 #853/#874 类的「忘失效」 |
| **R43** | TTL / 容量类新配置项默认值：bool 默认 false、string 默认空、int 默认 0 = 安全路径（沿用 AGENTS.md 18 条） | 零回归 |

---

## 四、失效协议（**唯一契约**，写路径必须遵守）

```
任何改变「卷内某路径的存在性/内容」的操作成功后（写内核 / 旁路写 / 卷内自管写）：
    cache.Invalidate(ObjectRef{Volume, Owner, Rel})
        └→ 删除 TreeView 该 rel 条目
        └→ 标记全部 ancestor 目录的 DirView 为 stale（gen 置 dirty，下次读回源）
        └→ 删除对应 staging JSON（best-effort；失败仅日志，stale 标记已足够）
```

**Rename**：`Invalidate(from)` + `Invalidate(to)`（两个 ref）。
**Delete（软删 → 回收站）**：`Invalidate(rel)`；恢复时再 `Invalidate(rel)`。
**批量写（转存/归档/镜像/同步 push）**：逐产出对象发一次（由统一写内核或旁路登记点保证）。

**写穿失败的处理**：`Invalidate` 返回 error 时**不阻断业务**（写已成功），但要
① 记 Warn ② 把该 `(vol,owner)` 标记为 **degraded**（后续读全部回源，直到显式 Refresh 成功）。
这避免「失效失败 ⇒ 永久幽灵」——降级为「慢但正确」。

---

## 五、迁移分期（每片独立可验证、可回退）

| 片 | 内容 | 行为变更 | 验收 |
|---|---|---|---|
| **V1** | `pkg/volumestate` 骨架：`ObjectRef`/`DirSnapshot`/`TreeEntry`/`Cache`（内存 LRU + staging 读写 + 原子写）、TTL/代际判定、`Invalidate`/`Refresh`；用 `syncmock`/`LocalFS` 做单测 | 无（未接线） | 单测覆盖：命中/miss/TTL 过期/代际变化/写穿失效/祖先 stale/degraded |
| **V2** | **外部卷接入**：`files.List(?volume=外部卷)` 改走 `cache.ListDir`；`ExternalList` 保留为回源函数；TTL 30s；secretdata 走代际 | 外部卷 list 首次慢、之后命中；外部改写 ≤30s 收敛 | e2e：外部卷 list 两次（第二次命中、断言无远端往返）；`Refresh` 后立即生效 |
| **V3** | **本地卷接入**：`files.List`/`Search` 读路径改走 `cache`；`searchIndex` 进入「双读兼容期」（读以 cache 为准，写仍双写） | list/search 结果不变 | 全量 `pkg/files` + `pkg/server` 绿；`checksums.json` 只读回退可用 |
| **V4** | **收敛写侧**：`RecordBypassWrite`/`IndexMove` 改为 `cache.Invalidate` + `cache.Upsert`（同一入口）；删除 `searchIndex` 与 `checksums.json` 的独立写路径；`checksums.json` 迁移为一次性 import（首次启动） | 无（行为等价） | #874 的回归用例仍绿；变异（删 Invalidate）→ 红 |
| **V5** | **门禁 + 文档 + 归档**：R40-R43；`docs/architecture.md` 增「卷状态维护」章；`docs/archive/volume-state-maintenance.md` 精华；删除 searchIndex/ExternalList 死代码 | 无 | `make check-ci` 全绿；deadcode 门禁通过 |

**回退**：每片一个 commit；V2/V3 用装配开关（`volume_state_enabled`，默认 false = 旧路径）控
灰度——开关关闭即完全旧行为。

---

## 六、安全性与边界审查

| 面 | 风险 | 对策 |
|---|---|---|
| 保密卷元数据落盘 | 加密卷的 name/checksum 落节点本地明文 | `state_cache_persist_private` 默认 **false**（只内存）；显式开启才落盘并文档标注威胁模型 |
| checksum 被缓存污染 | A 类安全语义被削弱 | 三段式兜底（§3.2.5）+ 写穿 + 代际；缓存永不作删除门禁的唯一依据 |
| 失效失败 | 永久幽灵 | degraded 降级为「全部回源」（§四） |
| staging 目录膨胀 | 磁盘占用 | LRU + `state_cache_max_bytes` 上限 + 启动 tmp 清理 + 卷删除回收 |
| 多节点 | 各节点缓存不一致 | 节点本地 + TTL 收敛（与容量账本同口径）；写穿只在产生写的节点生效——**文档明确**：跨节点可见性 ≤ TTL |
| 递归 search 打远端 | 大目录全量拉取 | `volume_state_search_remote` 默认 **false**（远端卷不提供 search，显式开启才启用） |
| 与配额/容量账本混淆 | 双账本 | 明确 D 类不进入本层（§二非目标） |

---

## 七、验收矩阵（关键断言）

| 断言 | 层 | 变异（应红） |
|---|---|---|
| 同一目录二次 list，第二次不回源（计数器） | V2 | 去掉 LRU 命中判断 |
| 外部卷 TTL 过期后回源一次 | V2 | 去掉 TTL 判定 |
| 卷 `StateGen` 变化 → 强制回源 | V2 | 去掉代际比对 |
| `Invalidate(ref)` 后该 rel 立即不可见 | V2/V3 | 去掉 TreeView 删除 |
| 写子目录后**父目录** list 反映新条目 | V3 | 去掉祖先 stale 标记 |
| `Invalidate` 失败 → 该卷后续读全部回源（degraded） | V2 | 去掉 degraded 标记 |
| 保密卷不落盘（staging 目录无该卷文件） | V2 | 去掉 `volumeHasPrivate` 分支 |
| 转存产物（#874 场景）list 可见 + checksum 非空 | V3/V4 | 删掉写后 Invalidate/Upsert |
| 删除后 list 不再返回该条目（无幽灵） | V3 | 去掉 Delete 的 Invalidate |
| 门禁 R40：新增 list 级缓存 → 红 | V5 | — |

---

## 八、明确不覆盖

1. **D 类聚合账本**（配额用量/卷容量）——已有 `pkg/volume/capacity` 独立对账体系，不并入。
2. **内容缓存**（secretdata 的 span/blocklet/window 缓存）——那是**内容字节**缓存（性能），
   与本层的**元数据视图**缓存职责不同；它们继续由卷自管，本层不接管。
3. **跨节点强一致**——节点本地 + TTL 收敛是明确口径（与容量账本一致）；不引入分布式协调。
4. **鉴权/审计派生状态**——凭据 store、审计 journal 有独立生命周期，不进入本层。
