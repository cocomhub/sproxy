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
    StateGen(ctx context.Context, owner, rel string) (uint64, error)
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
func (c *Cache) StatObject(ctx, vol, owner, rel string) (sync.Entry, bool, error)
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

**失效失败不阻断业务**（写已成功）：记 Warn + 该 `(vol,owner)` 标 **degraded**（后续读全部
回源，直到显式 `Refresh` 成功）⇒ 降级为「慢但正确」，不留永久幽灵。

---

## 五、迁移优先级（用户裁定 ③：外部卷最优先，本地卷可共存）

> 关键纪律：**共存不等于两套并行**——旧实现被登记为**只减不增的棘轮**（R46），
> 新代码一律走新机制。这样「短期共存」不会演变成永久双轨。

| 片 | 内容 | 优先级 | 行为变更 | 验收 |
|---|---|---|---|---|
| **V1** | `pkg/volumestate` 骨架：`ObjectRef`/`DirSnapshot`/`TreeEntry`/`Cache`/`MutationBus`/`WrapVolume`；staging 原子读写 + LRU + TTL/代际判定；`storage.Root` 变异观察者（不接线） | 高 | 无 | 单测：命中/miss/TTL/代际/写穿/祖先 stale/degraded/保密卷不落盘 |
| **V2** | **外部卷接入（本期核心）**：装配期 `WrapVolume` 包所有非本地卷；`files.List/Stat/Delete/Rename(?volume=)` 走装饰器；TTL 30s；secretdata 用 `StateGen`；`ExternalList/ExternalStat` 保留回源 | **最高** | 外部卷 list 首次慢、之后命中；外部改写 ≤30s 收敛 | e2e：二次 list 无远端往返（计数）；TTL 过期回源一次；`Refresh` 立即生效；删除后无幽灵 |
| **V3** | **本地卷迁移**（可延后）：`searchIndex` 收敛为 TreeView 投影；`checksums.json` 一次性 import 后转只读；Root 事件接线 | 中（后续） | list/search 行为不变 | 全量绿；旧路径棘轮计数下降 |
| **V4** | 清理：删 `searchIndex`/`checksums.json` 独立写路径与 `ExternalList` 薄转发；`RecordBypassWrite`/`IndexMove` 删（由事件总线取代） | 低 | 无 | #874 回归用例绿；deadcode 门禁过 |
| **V5** | 门禁 + 文档 + 归档：R40/R41/R42/R44/R45/R46 | 中 | 无 | `make check-ci` 全绿 |

**灰度**：`volume_state_enabled`（默认 **false** = 旧行为）。V2 合入后由运维逐环境开启；
开关关闭时 `WrapVolume` 原样返回。

---

## 六、门禁清单（机制稳定性的保证）

| 门禁 | 判据 | 防的是 |
|---|---|---|
| **R40** | 非测试源码除 `pkg/volumestate` 外不得出现 list 级缓存类型（`*Cache`/快照/索引 map + list 语义）；`pkg/volume/secretdata` 自管索引登记白名单（理由：内容是密文，只有卷能算） | 再造第四套 |
| **R41** | `files.List/Search` 实现体不得直调 `index.list` / `ExternalList`（V4 后） | 双入口 |
| **R42** | 新增 `sync.FS` 实现若持有派生状态，必须实现 `StateGeneration` 或自述「无状态」注解 | 忘失效（#853/#874 类） |
| **R44** | `volumestate.WrapVolume` 只在装配处出现；业务代码不得自造 FS 缓存包装 | 绕过唯一入口 |
| **R45** | `storage.Root` 新增变异方法必须回调 `notifyMutation` | 机制因原语演进失效 |
| **R46** | **旧实现棘轮**：`searchIndex`/`checksums.json`/`ExternalList` 的独立使用点数（`*.tsv` 登记）**只减不增** | 「短期共存」变永久双轨 |
| **R43** | 新增配置项默认值：bool=false、string=空、int/duration 取安全默认（AGENTS.md 18 条） | 零回归 |

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
