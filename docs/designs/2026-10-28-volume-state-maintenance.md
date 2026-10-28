# 统一卷状态维护：卷维度 + 装饰器机制（设计）

> 状态：**设计（待实施）**。来源：2026-10-28 用户明示 ——
> ①「设计完善可靠的所有卷状态维护方案，支持收敛 checksum、list、索引等信息维护，避免不同卷
> 多套实现（secretdata 等支持自行管理，但默认也有基于本地 staging 的缓存提高性能）」；
> ②「设计成架构机制，避免需要频繁修改」；
> ③「后续所有业务操作都必须是卷维度；本地卷旧实现可短期共存、后续迁移；最优先外部卷管理」。
>
> 前置：`docs/designs/2026-10-27-remote-derived-state-cache.md`（A/B/C/D 分类）、
> `docs/designs/2026-10-28-unified-volume-model.md`（StorageVolume + `VolumeView` 唯一取卷入口）。
> 触发案例：PR #874（旁路写不登记派生状态 ⇒ 转存产物 list 不可见 + checksum 空）。

---

## 一、裁定：为什么**不**做「专门的缓冲卷」

被否决的方案：新增卷后端类型（`type: cache` / 缓冲卷），包在源卷前面提供 list/checksum 缓存。

| 否决理由 | 具体后果 | 与本设计的对比 |
|---|---|---|
| **默认不生效** | 卷型必须被配置声明才存在 ⇒ 所有存量外部卷仍「每次远端 ListDir」⇒ 最痛点不解决；新增卷要逐卷改配置 = **频繁修改** | 装饰器在**装配期自动**包住每个卷 ⇒ 零配置、零声明 |
| **污染卷命名空间** | `cache` 卷会出现在 `/api/volumes`、ACL、`owner_quotas`、move/copy/rebalance、`/api/resolve`、转存目标下拉、mirror/backup 目标……**每个卷面 API 都要加「排除 cache 卷」守卫** ⇒ 这是**永久的持续修改负担** | 装饰器不是卷 ⇒ 卷命名空间、ACL、配额、路由全部零改动 |
| **破坏 `Kind` 不变量** | 缓冲卷拥有本地字节 ⇒ `KindStorage` ⇒ 按既有设计必须注册配额层 ⇒ 与「缓存不占用户配额」冲突，需新增 Kind 或写特例 | 装饰器不改变 `Kind`/配额语义；缓存落盘区由单一组件按 `state_cache_max_bytes` 独立管理 |
| **失效仍需纪律** | 卷型只解决「读加速」；写后失效仍要求每个写路径自觉通知 ⇒ #874 类缺陷（漏一处即漂移）原样存在 | 本设计的失效来自**变更事件总线**（见 §四）⇒ 写路径无需知道缓存存在 |

**成功先例（同形态问题、已解决的解）**：容量层当年也是「5 个装配点各自的义务 ⇒ 漏一处静默
无限额」，解法**不是新卷型**，而是唯一入口 `capacity.WrapStorageVolume` + **R29 门禁**
（非测试源码不得直接调 `WrapBackend`）。本设计采用**同一形状**：唯一入口
`volumestate.WrapVolume(...)`（§3.3）+ **R44 门禁**。

> 「缓冲」在本设计中仍然存在，但它是**受管落盘区**（`<storage_root>/meta/state/<卷>/…`，
> 与容量账本 `<root>/meta/quota/<卷>` 同约定），由 `pkg/volumestate` **单一组件**负责
> 上限/GC/保密卷不落盘——**不是卷模型里的实体**。

---

## 二、架构原则（长期稳定的不变量）

> 这四条是「机制不需要频繁修改」的保证：新卷型、新写路径、新业务面都自动满足。

### P1 卷维度（用户裁定 ③）

**所有卷状态以「卷」为第一维**：键恒为 `(volume, owner, rel)`，**不存在 owner 级全局卷状态**。

- 跨卷聚合视图（如「列出 owner 所有文件」）是**对卷集合的派生遍历**，不是一个独立的全局索引。
- 新业务操作必须显式携带/解析卷（`VolumeView(name, use)` 已是唯一取卷入口）。
- 推论：缓存天然按卷分区 ⇒ 卷删除/卸载即整分区回收，无需全局失效广播。

### P2 横切关注点 = 装配期装饰器，不是调用点义务

状态缓存与容量记账同类（横切）：在**装配期唯一入口**包一层，消费方无感知。
**禁止**任何业务代码直接操作缓存（只经 `VolumeView`/`StorageVolume.FS()` 拿到的已是装饰后视图）。

### P3 失效由「变更事件」驱动，不靠调用方登记

写路径**不需要知道缓存存在**。变更事件有两个**原语级**产生点（覆盖全集，见 §四）：
`sync.FS` 写方法（外部/封装卷）+ `storage.Root` 写方法（本地卷）。任何新写路径必落在其一。

### P4 卷自管 = 只报告事实，不实现缓存

卷可实现可选能力如实报告状态（`ListDir` 填 checksum、`StateGeneration` 报告代际），
但**不得**自己实现 list 缓存（门禁 R40）。加速统一由 `pkg/volumestate` 提供 ⇒ 自管卷与普通卷
共享同一条缓存路径，不产生第二套。

---

## 三、目标架构

```
┌──────────────────────────────────────────────────────────────────────────┐
│ 业务面（卷维度，P1）：files.List/Search/Stat、转存、归档、move/copy、     │
│ mirror、backup、DAV/S3 —— 全部经 VolumeView/StorageVolume.FS()           │
└────────────────────────────┬─────────────────────────────────────────────┘
                             │ 拿到的是**装饰后** FS（P2，无感知）
┌────────────────────────────▼─────────────────────────────────────────────┐
│ 装配期装饰链（唯一入口，R44）：                                          │
│   capacity.WrapStorageVolume（既有：卷级容量）                            │
│   volumestate.WrapVolume（新增：状态缓存 + 变更事件）                     │
│     ├─ 读：ListDir/Stat → Cache（命中/回源/新鲜度校验）                    │
│     └─ 写：WriteFile/Delete/Rename/MakeDir → 委派 + **发变更事件**        │
└────────────────────────────┬─────────────────────────────────────────────┘
                             │ 回源 / 事件
┌────────────────────────────▼─────────────────────────────────────────────┐
│ pkg/volumestate（唯一实现）                                              │
│   Cache      : (vol,owner,rel) → DirSnapshot{entries,gen,fetchedAt}      │
│   TreeView   : (vol,owner) → 全树投影（search；由 DirSnapshot 拼装）       │
│   落盘区     : 受管 staging `<root>/meta/state/<vol>/…`（上限/GC/保密不外落）│
│   MutationBus: ObjectRef 事件 → 失效（P3）                               │
└────────────────────────────┬─────────────────────────────────────────────┘
                             │ 卷自报事实（P4）
┌────────────────────────────▼─────────────────────────────────────────────┐
│ 卷侧契约（既有 sync.FS，不新造）：                                        │
│   ListDir/Stat → Entry{Checksum, Checksums, ChecksumType}（已有通道）     │
│   + 可选 StateGeneration（廉价代际：本地卷=目录mtime；secretdata=meta版本；│
│     S3=LastModified；baidupcs=ctime+条目数）                              │
└──────────────────────────────────────────────────────────────────────────┘
```

### 3.1 卷侧契约（P4：只报告，不缓存）

**必需**（已存在，零新增）：`sync.FS.ListDir/Stat`，`Entry` 尽可能填
`Checksum/Checksums/ChecksumType`（契约沿用既有设计 §3.1：「零额外成本必须填」；远端逐文件
RTT ⇒ 显式留空 = unknown）。

**可选**（新增，能力接口风格与 `IndexRefresher` 一致）：

```go
// pkg/volume（域模型包，不依赖 pkg/sync）
// StateGeneration 是卷的**廉价新鲜度探针**：返回 (owner, rel) 子树的代际号。
// 同代际 ⇒ 该子树的结构视图与内容派生值自上次取用后未变。
// 实现必须廉价（不得全量遍历）：本地卷=目录 mtime+子项数；secretdata=容器 meta 版本；
// S3=子树 LastModified 最大值；baidupcs=ctime+条目数。
type StateGeneration interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
    // path 是**卷根相对路径**（与 sync.FS 键空间一致，首段即 owner）。
    StateGen(ctx context.Context, path string) (uint64, error)
}
```

未实现 ⇒ 该卷退化：外部卷 TTL-only、本地卷写穿-only（= 现状零回归）。

### 3.2 统一缓存 `pkg/volumestate`（唯一实现）

#### 3.2.1 数据模型（卷维度，P1）

```go
// ObjectRef 是失效信号的最小单元；Volume 是第一维（P1）。
type ObjectRef struct {
    Volume string // 卷名（必填；"" 仅默认卷兼容期）
    Owner  string
    Rel    string // 卷内相对路径（正斜杠，不含 owner 前缀，与 sync.FS 键空间一致）
}

type DirSnapshot struct {
    Entries   []sync.Entry `json:"entries"`
    Gen       uint64       `json:"gen"`
    FetchedAt int64        `json:"fetched_at"`
    Volume    string       `json:"volume"` // 归属校验（防串卷）
}

type TreeEntry struct {
    Rel, Base    string
    Size, ModTime int64
    IsDir        bool
    Checksum     string `json:"checksum,omitempty"`
    ChecksumType string `json:"checksum_type,omitempty"`
    Volume       string `json:"volume"`
}
```

#### 3.2.2 API（消费方与装饰器唯一依赖面）

```go
func (c *Cache) ListDir(ctx, vol, owner, rel string) ([]sync.Entry, error)
// 说明：Stat **不进缓存**（见下「实现对齐」）——安全判定必须新鲜，且单对象成本远低于目录列举。
func (c *Cache) Search(ctx, vol, owner, query, tag string) ([]TreeEntry, error)
func (c *Cache) Invalidate(ref ObjectRef) error          // 事件总线调用（§四）
func (c *Cache) Refresh(ctx, vol, owner string) error    // 既有 POST /api/volumes/refresh
```

#### 3.2.3 落盘区（受管 staging，非卷实体）

```
<storage_root>/meta/state/<卷名>/<owner>/<dirRel>/entries.json   # 原子写 tmp+rename
<storage_root>/meta/state/<卷名>/<owner>/tree.json               # 全树投影（可选）
```

- 节点本地（与容量账本 `<root>/meta/quota/<卷>` 同约定）；**不进卷、不进网盘**
- 进程内 LRU（默认 4096 目录项）在 staging 之上
- 上限 `state_cache_max_bytes`（默认 256 MiB；**0 = 只内存不落盘**）；超限 LRU 淘汰；
  启动清理 `*.tmp`；卷删除整分区回收
- **保密卷**：只存元数据（name/size/mtime/checksum），不含内容；对 `volumePrivate()` 卷
  默认**不落盘**（`state_cache_persist_private` 默认 false，反义命名遵循 AGENTS.md 18 条）

#### 3.2.4 新鲜度判定（三重，A 类强一致）

```
命中：snapshot = LRU.Get ?? disk.Load
  snapshot == nil                    → 回源
  now - FetchedAt > ttl              → 回源
  卷实现 StateGeneration:
     gen, err = vol.StateGen(...)
     err != nil                      → 回源（探测失败用真值，不用可能过期的缓存）
     gen != snapshot.Gen             → 回源
  返回 snapshot.Entries
```

TTL 分档（`volume_state_ttl`，默认按卷类型）：

| 卷类型 | 默认 | 依据 |
|---|---|---|
| **外部卷**（baidupcs/S3/webdav/ftp/sftp/federated/cluster） | **30s** | 本期最优先；避免大目录反复远端往返 |
| 自管卷（secretdata） | 30s + 代际 | 卷内写即时更新自身索引；跨进程由代际发现 |
| 本地卷（本期仍走旧实现，见 §五） | 0（不按时间过期） | 写穿 +（未来）目录 mtime 代际 |
| 禁用 | `-1` | 每次回源（调试/强一致） |

**A 类兜底（永不依赖缓存齐全）**：需要 checksum 才安全的写操作走三段式——① 调用方给了用；
② 没给 `StatObject` **现取**（本地卷直接回源，不取缓存值）；③ 仍拿不到 **fail-closed 拒绝**。

### 3.3 唯一装配入口 + 门禁（复制 R29 形状）

```go
// pkg/volumestate
// WrapVolume 是卷状态缓存的**唯一构造入口**（与 capacity.WrapStorageVolume 并列、同一装配点）。
// 它把「读缓存 + 写事件」绑定为一个原子动作 ⇒ 不可能只做一半。
// nil cache（未启用）⇒ 原样返回（零回归）。
func WrapVolume(sv registry.StorageVolume, c *Cache) registry.StorageVolume
```

门禁 **R44**：非测试源码中 `volumestate.WrapVolume` 只能出现在装配处；
卷的 `FS()` 结果不得被业务代码再包一层「自造缓存」。

### 3.4 消费入口（卷维度）

| 消费方 | 本期 |
|---|---|
| `files.List(?volume=<外部卷>)` / `Stat` / `DeleteFile` / `Rename` | 走装饰后 FS ⇒ 自动缓存 + 自动失效 |
| `ExternalVolumeOps.ExternalList/ExternalStat` | 语义并入装饰器；保留兼容期薄转发 |
| `files.Search`（外部卷） | 默认不支持（`volume_state_search_remote` 默认 false）；启用则读 TreeView |
| 本地卷 list/search | **本期保持旧 `searchIndex`**（用户裁定允许共存，§五 V3 迁移） |

---

## 四、失效机制：变更事件总线（P3，**不靠纪律**）

### 4.1 事件产生点（全集且稳定）

| 产生点 | 覆盖 | 观察方式 |
|---|---|---|
| `sync.FS` 写方法（`WriteFile`/`Delete`/`Rename`/`MakeDir`） | **外部卷、封装卷、secretdata**（全部非本地卷） | `volumestate.WrapVolume` 的 FS 装饰器在委派成功后发事件 |
| `storage.Root` 写方法（`OpenFile*` 关闭、`Remove`、`RemoveAll`、`Rename`、`AtomicRename`、`MkdirAll`、`Link`、`Chtimes`） | 本地卷（写内核直用 Root） | Root 构造时注入 `MutationObserver`；每次成功后回调 |

**为何这两个点是全集**：任何改变卷内容的路径只能落到「卷的 FS」或「本地卷根 Root」
（`pkg/server` 的 `Set.Root` 访问器已被 R28 棘轮登记，共 12 处且只降不升；其余写路径均经
`VolumeView`/`FS()`）。**新写路径**不需要任何登记——它必然经过其中之一。

**稳定性守卫（R45）**：`storage.Root` 的导出方法若新增且属变异类（名在变异集合内），
门禁要求其调用 `rt.notifyMutation`（漏写即红）⇒ 机制不会因 Root 演进悄悄失效。

### 4.2 失效语义

```
Invalidate(ObjectRef) →
  ① 删除 TreeView 该 rel 条目
  ② 标记全部 ancestor 目录 DirView 为 stale（下次读回源）
  ③ best-effort 删除对应 staging JSON（失败仅日志，②已足够）
Rename → Invalidate(from) + Invalidate(to)
Delete/软删/恢复 → Invalidate(rel)
批量（转存/归档/镜像/同步）→ 逐产出对象各发一次（装饰器天然覆盖）
```

**失效失败不阻断业务**（写已成功），且**实现上不需要 degraded 标记**：内存 LRU 是失效的
一等公民——同进程内失效不可能失败；磁盘快照删除是 best-effort，陈旧快照另有 TTL 与代际兜底
（本地卷则本就只依赖写穿 + 代际）。这与「靠标记降级」相比更简单、无额外状态（实现见
`pkg/volumestate/cache.go` 的 `Invalidate`）。

**实现对齐（2026-10-28 落地时确认）**：
- `Stat`/`StatObject` **不进缓存**：A 类安全判定必须新鲜，且单对象成本远低于目录列举；
  缓存只服务 `ListDir`（C 类结构视图）。
- 代际探测沿装饰链向内找首个 `StateGeneration` 实现（装饰器当前未转发该能力 ⇒ 未实现即
  退化为无代际，TTL/写穿兜底，fail-safe）。

---

## 五、迁移优先级（用户裁定 ③：外部卷最优先，本地卷可共存）

> 关键纪律：**共存不等于两套并行**——旧实现被登记为**只减不增的棘轮**（R46），
> 新代码一律走新机制。这样「短期共存」不会演变成永久双轨。

| 片 | 内容 | 优先级 | 行为变更 | 验收 |
|---|---|---|---|---|
| **V1** | `pkg/volumestate` 骨架：`ObjectRef`/`DirSnapshot`/`TreeEntry`/`Cache`/`MutationBus`/`WrapVolume`；staging 原子读写 + LRU + TTL/代际判定；`storage.Root` 变异观察者（不接线） | 高 | 无 | 单测：命中/miss/TTL/代际/写穿/祖先 stale/degraded/保密卷不落盘 |
| **V2** | **外部卷接入（本期核心）**：装配期 `WrapVolume` 包所有非本地卷；`files.List/Stat/Delete/Rename(?volume=)` 走装饰器；TTL 30s；secretdata 用 `StateGen`；`ExternalList/ExternalStat` 保留回源 | **最高** | 外部卷 list 首次慢、之后命中；外部改写 ≤30s 收敛 | e2e：二次 list 无远端往返（计数）；TTL 过期回源一次；`Refresh` 立即生效；删除后无幽灵 |
| **V3** | **本地卷迁移**（可延后）：`searchIndex` 收敛为 TreeView 投影；`checksums.json` 一次性 import 后转只读；Root 事件接线 | 中（后续） | list/search 行为不变 | 全量绿；旧路径棘轮计数下降 |
| **V4** | 清理：删 `searchIndex`/`checksums.json` 独立写路径与 `ExternalList` 薄转发；`RecordBypassWrite`/`IndexMove` 删（由事件总线取代）——**阶段一已完成**（见 §十三），剩余待 `checksums.json` 只读化与 TreeView 投影 | 低 | 无 | #874 回归用例绿；deadcode 门禁过 |
| **V5** | 门禁 + 文档 + 归档：R40/R41/R42/R44/R45/R46 | 中 | 无 | `make check-ci` 全绿 |

**灰度**：`volume_state_enabled`（默认 **false** = 旧行为）。V2 合入后由运维逐环境开启；
开关关闭时 `WrapVolume` 原样返回。

---

## 六、门禁清单（机制稳定性的保证）

| 门禁 | 判据 | 防的是 |
|---|---|---|
| **R40** ✅已实现 | 非测试源码的 `*Cache`/`*Index` 结构体必须登记（`list_cache_types.tsv`，新增须复用 `pkg/volumestate` 或说明理由；secretdata 白名单理由：内容是密文，只有卷能算） | 再造第四套 |
| **R41** ✅影子台账 | `index.list`/`index.search`/`ExternalList` 直调点逐文件计数棘轮（`list_source_calls.tsv`，只减不增）；迁移完成后收敛为 0 即转硬禁 | 双入口 |
| **R42** ✅已实现 | 带 `ListDir` 的类型（`sync.FS` 实现）必须实现 `StateGen`，或登记在 `fs_derived_state.tsv` 说明「为什么不需要自述代际」 | 忘失效（#853/#874 类） |
| **R44** | `volumestate.WrapVolume` 只在装配处出现；业务代码不得自造 FS 缓存包装 | 绕过唯一入口 |
| **R45** ✅已实现 | `storage.Root` 结构性变异方法必须回调 `notifyMutation`（`internal/archcheck/root_mutation_gate_test.go`） | 机制因原语演进失效 |
| **R46** ✅已实现 | **旧实现棘轮**：`searchIndex`/`checksums.json`/`ExternalList` 的独立使用点数（`legacy_state_usage.tsv` 登记）**只减不增** | 「短期共存」变永久双轨 |
| **R43** ✅已实现 | 新增配置项默认值：bool=false、string=空（`config_defaults.tsv` 登记既有特化默认值，新增须登记理由） | 零回归 |

---

## 七、安全性 / 边界

| 面 | 风险 | 对策 |
|---|---|---|
| 保密卷元数据落盘 | 加密卷 name/checksum 落节点本地明文 | `state_cache_persist_private` 默认 false（只内存） |
| 缓存削弱 A 类语义 | 「缓存说一致、实际已变」 | 三段式兜底 + 写穿 + 代际；缓存永不作删除门禁唯一依据 |
| 失效失败 | 永久幽灵 | degraded 降级为全部回源（§4.2） |
| staging 膨胀 | 磁盘占用 | `state_cache_max_bytes` 上限 + LRU + 启动 tmp 清理 + 卷删除回收 |
| 多节点 | 各节点不一致 | 节点本地 + TTL 收敛（与容量账本同口径）；**文档明确**跨节点可见性 ≤ TTL |
| 递归 search 打远端 | 大目录全量拉取 | `volume_state_search_remote` 默认 false |
| 与配额账本混淆 | 双账本 | D 类不进入本层（§八） |

---

## 八、明确不覆盖

1. **D 类聚合账本**（配额/容量）——已有 `pkg/volume/capacity` 独立对账，不并入。
2. **内容字节缓存**（secretdata 的 span/blocklet/window 缓存）——那是**内容平面**，
   与本层**元数据平面**职责不同；继续由卷自管。
3. **跨节点强一致**——节点本地 + TTL 是明确口径，不引入分布式协调。
4. **鉴权/审计派生状态**——凭据 store、审计 journal 有独立生命周期。
5. **本地卷旧实现的立即移除**——按用户裁定可延后（V3/V4），由 R46 棘轮保证只减不增。

---

## 九、验收矩阵（关键断言 + 变异）

| 断言 | 片 | 变异（应红） |
|---|---|---|
| 同目录二次 list 不回源（计数） | V2 | 去 LRU 命中 |
| 外部卷 TTL 过期回源一次 | V2 | 去 TTL 判定 |
| `StateGen` 变化 → 强制回源 | V2 | 去代际比对 |
| `Invalidate` 后该 rel 立即不可见 | V2 | 去 TreeView 删除 |
| 写子目录后**父目录** list 反映新条目 | V2 | 去祖先 stale |
| 经 FS 的写入**无需显式登记**即失效（机制验证） | V2 | 去装饰器事件 |
| `Invalidate` 失败 → 后续读全部回源 | V2 | 去 degraded |
| 保密卷 staging 目录无该卷文件 | V2 | 去 private 分支 |
| 转存产物（#874 场景）list 可见 + checksum 非空 | V2 | 去事件失效 |
| 新增 `Root` 变异方法未通知 → R45 红 | V5 | — |
| 旧实现使用点数上升 → R46 红 | V5 | — |

---

## 十、V2 端到端验证结论（2026-10-28）

E2E（`test/e2e_volume_state_test.go`，真实 sproxy + 真实 sclient + 真实 WebDAV 外部卷）锁定：

1. **写面必须全部经 `VolumeView`**——`externalSinkFor`（外部卷上传写面）此前直取
   `External().FS()` ⇒ 上传不失效缓存（E2E 当场捕获：上传后 list 仍显示旧视图），已改为经
   `VolumeView`（并保留「仅外部卷」判定，避免本地卷误入外部写分支）。
2. **对照组**：未启用缓存时外部直改**立即可见**；启用后 TTL 内不可见、`POST /api/volumes/refresh`
   立即收敛 ⇒ 两个测试行为差异即「缓存真实生效」的证据（防假绿）。
3. **refresh 语义修正**：对无自管索引的外部卷，清空状态缓存即视为重建成功（此前恒 405）。

⇒ 新增写面时的稳定性要求（P3 的落地检查）：**任何读写外部卷的入口都不得直取
`External().FS()`**；已有 R27 门禁按次登记，后续片可把「外部卷写面必须经 VolumeView」并入 R27 判据。

---

## 十一、V3 阶段一：Root 变异事件 + 本地卷索引自动维护（2026-10-28 落地）

**交付**：

1. `storage.Root` 结构性变异事件（`pkg/storage/root_mutation.go`）：`MutationOp`（mkdir/remove/
   remove_all/rename/link/chtimes）+ `SetMutationObserver(tag, fn)`；事件**仅在底层操作成功后**发出，
   租户子根**继承并归一前缀**（`<owner>/<rel>`，`Root` 恒指向卷根）⇒ 消费方无需知道是哪个子根写的。
   未装观察者 = 零开销零回归。
2. 消费端 `files.Service.ApplyRootMutation`（`pkg/files/mutation_index.go`）：把事件落到搜索索引
   （path 翻译 / 各操作类型 / **只维护 `user/` 桶** / 索引未构建时零成本）。接线唯一入口
   `pkg/server/volume_mutation_wiring.go`（`fileService()` Once 内 + 运行期建卷同一函数）。
3. 门禁 **R45**（Root 变异方法必须 `notifyMutation`）与 **R46**（旧实现棘轮）落地。

**边界（诚实记录）**：`OpenFile`/`OpenFileEncrypted` 的**内容写入**不发结构事件——它们返回
`*os.File`/`io.WriteCloser`（30+ 调用点依赖具体类型），包装会破坏类型契约；内容写的 size/checksum
由写内核维护，落位仍经 `AtomicRename`（发 rename 事件）。

**已知代价**：普通上传路径现在会「写内核 upsert + 观察者 upsert」各一次（幂等）。V4 删除写路径的
逐点维护后回到一次。

**E2E/单测证据**：`pkg/storage/root_mutation_test.go`（6 方法事件/失败不发/租户前缀/AtomicRename）；
`pkg/files/mutation_index_test.go`（旁路写进索引、删除出索引、rename 搬迁、非 user 桶忽略、路径边界）；
`pkg/server/volume_mutation_wiring_test.go`（真实卷根旁路写 → 搜索自动命中；变异「不接线」即红）。

**未做（下一片）**：`checksums.json` 一次性 import 后转只读（校验和不可由结构事件推导，需迁移脚本
+ 读路径回落）；`searchIndex` 彻底替换为 TreeView 投影（`IndexSync`/集群快照/内容索引/向量索引耦合）。

---

## 十二、V3 阶段二：本地卷内容写事件（2026-10-28 落地）

阶段一（#880）覆盖了**结构性**变异（mkdir/remove/rename/link/chtimes），但文件**内容**写不经那里：
`rootFSCore.WriteFile` 是 `O_CREATE|O_TRUNC` 原地写（不落临时文件 + rename），`OpenFile` 又返回裸
`*os.File`（30+ 调用点依赖具体类型，包装会破坏类型契约）。

**落地**：`storage.Root.MarkWritten(rel)` + `MutationWrite`，由**卷的 FS 写面**在写成功后调用
（当前唯一调用点 = `pkg/rootfs.rootFSCore.WriteFile`，即转存/归档/镜像/备份落盘的共同入口）。
⇒ 本地卷的派生状态现在对「结构 + 内容」两类写都自动跟进，`RecordBypassWrite` 只在
checksum 台账维度仍是必需的（外部卷目标仍靠它登记索引，因其无本地 Root 事件）。

**代价（如实记录）**：转存到本地卷时索引会更新两次（`RecordBypassWrite` 一次 + 事件一次，幂等）；
V4 把 `RecordBypassWrite` 收敛为「只写 checksum」后回到一次。

**门禁 R42**（同期落地）：带 `ListDir` 的类型必须实现 `StateGen` 或登记理由——保证「有派生状态的
FS 实现能被缓存层探测到变化」，与 R40（不许再造一套）互补。

---

## 十三、V4 阶段一：旁路写只补 checksum（2026-10-28 落地）

`RecordBypassWrite`（checksum 台账 + 索引）此前是转存/归档的**唯一**派生状态入口。随着
Root 事件覆盖本地卷的「结构 + 内容写」（#880/#884），本地卷目标再调全量登记就是**重复 upsert**
（幂等，但白做一次整表 COW 克隆），并且对「卷内并不存在的路径」会造**幽灵索引条目**（`root=nil`
⇒ 没有 Stat 兜底）。

**落地**：

- `files.Service.RecordBypassChecksum(owner, rel, checksum)`：只写台账，不碰索引。
- `Handlers.recordBypassWrite`：装配层**统一入口**，按 `HasLocalRoot(volume)` 分流——
  本地卷 ⇒ checksum-only（索引交给事件）；外部/远端卷 ⇒ 保留全量 `RecordBypassWrite`
  （它们没有本地 Root 事件）。cloud manager 的 `RecordBypassWrite` 注入点与归档转存都走它。

**验收**：`TestService_RecordBypassChecksum_LedgerOnly`（台账命中 + 索引无幽灵）、
`TestRecordBypassWrite_LocalVolumeChecksumOnly`（本地卷幽灵=0；真实落盘后事件索引 + 台账 checksum
可见）。变异：把分流改回无条件全量 ⇒ 幽灵断言红。

**剩余（V4 其余）**：`ExternalList`/`IndexMove` 的薄转发删除依赖「本地卷 list 也走卷视图」
（= TreeView 投影）；`checksums.json` 只读化需先把校验和读路径切到对象 meta（见 §十四 计划）。

---

## 十四、V4 阶段二：跨卷搬移只迁移 checksum（2026-10-28 落地）

`IndexMove` 曾是跨卷 move 的「索引 + 台账」双登记入口。物理搬移必然经 `storage.Root`
（目标卷写 / 源卷删）⇒ 结构/内容写事件已经覆盖索引（源条目移除、目标条目与**卷归属**更新）。
本片把 `IndexMove` 收敛为**只迁移 checksum 台账**（事件观察不到的只有「键→值」迁移）。

**验收**：`TestIndexMove_MigratesChecksumAndEventDrivesIndex`——① `IndexMove` 写台账；
② 目标卷事件（`MutationWrite`，Tag=目标卷）后 list 条目带 checksum 且 `volume` 归属正确；
变异：去掉台账写入 ⇒ 红；去掉事件 ⇒ 条目缺失/卷归属陈旧 ⇒ 红。R46 棘轮同步下调
（`pkg/files/service.go` 的旧实现使用点 3 → 1）。

## 十五、剩余两项（需要设计决策，未在本期实现）

### 15.1 `checksums.json` → 对象 meta 为准（台账只读化）

现状：台账是 per-tenant 单 JSON（`meta/checksums.json`，整体重写、可丢可损）；对象 meta sidecar
已带 `TotalSHA256`（`pkg/files/meta`）。目标：**读以对象 meta 为准，台账退化为导入源**。

可行切片（按序）：
1. ~~自愈读~~ **✅ 已实现（步骤 ①+② 合并）**：`checksumSnapshot` 发现台账为空（丢失/损坏/新租户）
   即触发 `Service.RebuildChecksums`——逐卷遍历 `<owner>/user/` 从 sidecar `TotalSHA256`
   **批量回填**（`ChecksumStore.SetMany` 只落盘一次），每 owner 只尝试一次（`checksumRebuild`
   sync.Map 防列表路径反复全量遍历）。选「批量回填」而非「逐条目兜底」是为了避免大目录列表的
   N 次 sidecar I/O。验收：`TestService_ChecksumRebuildFromMeta`（删台账 → list checksum 自愈）
   + `_NoMetaNoCrash`（无 meta 能力零回归）；变异：去掉自动重建 ⇒ 红。
2. **增量自愈**（未做）：台账**部分**缺失（非全空）时目前不触发重建；需要的话可做
   `sclient checksum rebuild` 显式命令（走同一 `RebuildChecksums`）。
3. **台账转只读**（未做）：`Set/Delete/Rename` 不再写台账、读路径全量走 sidecar 或内存视图，
   删 `checksums.json` 写路径与 R46 对应计数。**前置**：跨卷同名键空间口径（见上）。

风险/决策点：台账键是**租户相对 rel（跨卷同名可冲突）**——对象 meta 是**卷内对象级**，两者键空间
不同，导入时必须按 volume 归属写入（否则跨卷同名文件互相覆盖）。这是本项必须先定的口径。

### 15.2 `searchIndex` → TreeView 投影

现状：本地卷 list/search 走 `searchIndex`（含内容索引、向量索引、集群 `IndexSync` 快照/rev、
持久化快照）。目标：list 由「卷状态视图（卷 FS ListDir + `volumestate` 缓存）」投影，searchIndex
只保留搜索语义。

为何未在本期做：`searchIndex` 同时承担 ① list 数据源、② 搜索（子串/内容/向量）、③ 集群索引同步
（rev/dirty/StateStore）、④ 快照持久化。把它拆成「结构视图（可投影）+ 搜索索引（需保留）」需要先定：
- 结构视图的来源（本地卷 `FS.ListDir` 是否足以支撑排序/分页/卷聚合）；
- 集群同步是否改为「结构视图同步 + 搜索索引本地重建」；
- 内容索引/向量索引的失效边界（新增/删除文件时增量维护），
  以及 R41 门禁（`files.List/Search` 不得直调 `index.list`/`ExternalList`）的落地时机。

建议按「先加 R41 的**影子台账**（记录调用点与迁移进度）→ 再逐卷切换 list → 最后删 index.list」
三步推进，每步都可用现有 list/search 全量用例回归。

**步骤 1 已落地（R41 影子台账）**：`internal/archcheck/list_source_gate_test.go` + `list_source_calls.tsv`
（3 文件 / 5 处基线：`pkg/files/read_ops.go` 3、`options.go` 1、`pkg/server/files_service.go` 1）。
只减不增；迁移完成后计数归零，届时把本门禁改成硬禁（禁止直调）。
