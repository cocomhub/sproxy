# 配额卷层（Quota Volume Layer）设计

> 2026-10-16 · 状态：**待实施（片 0 可先行）** · 前置阅读：`2026-10-14-quota-model-v3.md`（记账口径）、
> `2026-10-11-enclosure-volume.md`（封装卷机制）、`2026-10-12-user-write-kernel.md`（统一写内核）

---

## 0. 根因（本设计要消灭的缺陷类）

记账是**横切关注点**，却被实现成 **N 个调用点各自的义务**，且「逻辑↔物理」的换算是这个义务里
**唯一不可从类型系统发现**的一环：

| 漂移类 | 机制 | 本会话实证 |
|---|---|---|
| **① 单位不一致** | 每条写路径各自 `TryReserve/Commit`，尺寸取它**天然知道**的值（声明值 / `LogicalSize` / `userVisibleSize` ⇒ **逻辑**），而磁盘真值是**物理** | 备份 ✓ S3 multipart complete ✓ 跨卷 move ✓ sync pull ✓ —— **4 例同形态** |
| **② 释放 ≠ 结算** | 结算与释放是**两个**调用点，必须人工保证同口径 | `prevCharge` / `chargedHint` / quarantine 改名 / 归档 `actual` |

**判据（可复用）**：凡「写经**加密入口**（`writeUserVisible`/`OpenFileEncrypted`）+ 尺寸来自**逻辑**来源」
⇒ 必然漂移；凡尺寸取自**磁盘真值**（`root.Stat`）⇒ 天然同源。**根因不是某处写错，而是换算义务落在了错误的高度。**

---

## 1. 架构（用户裁定方向）

### 1.1 分层：配额层只包**存储卷**

```
用户/协议面（HTTP/DAV/S3/CLI/同步/云任务/备份）
        ↓  （**不做任何配额记账**）
封装卷（secretdata / trusted / 镜像 / 压缩…）   ← 视图层：只做变换，**零配额代码**
        ↓  （经存储卷接口读写 —— 见 §3 前置 P0）
┌──────────────────────────────────────────┐
│  存储卷（local 目录 / 外部后端）            │
│    └─ **QuotaVolume**（装配时自动包一层）   │
│         ├─ 每用户配额（owner → Scope）      │
│         ├─ 整卷配额（卷容量池）             │
│         └─ 足迹 = 存储卷自报（天然物理）     │
└──────────────────────────────────────────┘
```

**核心不变量**：*配额层与「真实字节」之间**没有变换层*** ⇒ 它看到的尺寸**天生是物理**，
**无需知道卷是否加密**（加密发生在存储卷内部 ✓；`root.Stat` 返回密文长度 ✓）。

### 1.2 卷类型显式化（`Kind` flag）

现状：wrapper 与 storage 的区分是**隐式**的 —— `enclosure.IsDeclared(v)` = `LookupWrapper(v.Type)` ✓
且 `v.Extra["source"] != ""` ✓（`pkg/volume/enclosure/enclosure.go:236`）。

**改为显式**（`volume.Volume` 是结构体 ⇒ 加方法）：

```go
// pkg/volume/volume.go
type Kind uint8

const (
    KindStorage Kind = iota + 1 // 可读写、承载字节、**需配额层**
    KindWrapper                 // 视图层：依赖某个存储卷，**不承载独立字节**
)

// Kind 返回卷的**结构性类型**（装配期解析一次，运行期只读）。
// 判据唯一来源：`Extra[enclosure.SourceKey]` 非空 ⇒ Wrapper；否则 Storage。
// 空 `Type` 与 `TypeLocal` 等义（既有裁定，零迁移）。
func (v Volume) Kind() Kind
```

**收益**：① 配额层装配点无需 import `enclosure`（解耦 ✓）；② 门禁可断言「Wrapper 不得注册配额层」
「Storage 必须有配额层」（覆盖封闭性 ✓）；③ 消除 `IsDeclared` 的多处隐式判据 ✗。

### 1.3 配额卷的职责（用户配额 + 整卷配额）

`QuotaVolume` 是**存储卷的唯一配额权威**，提供：

```go
// 整卷（卷容量池）——已有 `registry.Set.Pool(name)` 语义，收敛到本层
VolumeUsage() / VolumeCapacity() / SetVolumeCapacity(n)

// 每用户（owner 维度）
UserUsage(owner string) int64
Reserve(owner string, n int64) (*Reservation, error)   // 严格上界预占（不足排队）
Settle(owner string, prev, written int64)              // 结算（差分）
Release(owner string, n int64)                         // 释放
Footprint(owner, rel string) (int64, bool)             // **存储卷自报**（物理）
MaxPhysical(owner, rel string, logical int64) int64    // 写前上界（= 该卷变换的上界）
```

**为什么 `Footprint`/`MaxPhysical` 必须由存储卷实现**：加密 = 公式可算 ✓；外部卷 = 远端 stat ✓；
**没有一种卷的物理尺寸能由调用方推出** ✗ ⇒ 公式**只写在卷里**（今天散在 `files.settleSize` /
`physChargeFor` / `volume.PhysFootprinter` / `storage.EncryptedPhysicalSize` ✓ ⇒ 收敛 ✓）。

### 1.4 封装卷：**零配额代码**

封装卷（secretdata 等）**不承载独立字节** —— 它的一切读写都发生在某个存储卷上 ✓。
⇒ 它**只调用配额卷提供的能力**，不自己记账：

- 直接路径可归属时：由存储卷配额层按**路径首段**归属（`<owner>/<bucket>/<rel>` ✓）
- 路径不可归属时（**secretdata 的随机容器名 + 内容哈希** ✗，匿名性三维是安全特性 ✓）：
  封装卷**显式**把归属下传（见 §4 的 `LogicalKey` 通道 ✓）

⇒ 本设计**消灭**「每个卷自己实现足迹公式」✗（`VolumePhysFootprinter` 的动机消失 ✓）。

### 1.5 配额查询联邦化

现状：owner 配额是**进程内集中树**（`Handlers.quotaScopeFor` ✓ + `quotaBuckets` ✓）✗ ⇒
① 跨进程不可见 ✗；② 与「每卷独立配额」的 v3 裁定**语义重复** ✗。

**改为**：用户配额**按卷**持有（在各卷的配额层 ✓）；**用户视图 = 对「该用户可访问的卷」求和** ✓
（`AllowedVolumes(owner)` ✓ 已存在 ✓）⇒ 新增/改造查询端点，逐卷读取 + 汇总 ✓。

**收益**：① 消灭跨进程盲区（账本天然属于卷 ✓）；② 与 v3「每卷独立配额、用户每卷独立」**结构一致** ✓；
③ 卷删除/迁移时账本随卷走 ✓（今天 `delete+recreate` 的伪 507 类问题结构性消失 ✓）。

---

## 2. 正确性审查（含**上一版方案的硬错更正**）

### 🔴 更正：配额层**不能**靠「包装 reader 计数」

**实测**：`RootFS.WriteFile` → `writeUserVisible(root,…)` → 加密卷走 **`OpenFileEncrypted`** ✓
⇒ **变换发生在 `storage.Root` 内部** ✓ ⇒ 配额层若包在 root **之上**，数到的只是**明文** ✗。

**修正** ✓：配额层不是「字节计数器」，而是「**存储卷足迹的唯一消费者**」——
写前问 `MaxPhysical` ✓、写后/删前问 `Footprint` ✓（两者都由卷实现 ✓）。
（仅当配额层位于**变换之下**（即包装实际文件句柄 ✓）才能计数 —— 但那会**丧失归属**（§4）✗ ⇒ 不采用 ✓。）

### 其余 3 项必须成立

1. **上界预占**：写前必须拿到**严格上界**（G1 的机制 ✓）。secretdata 的精确尺寸**写前不可知** ✓
   ⇒ 「**上界预占 → 实测结算**」（设计已定 ✓）。
2. **归属通道**：见 §4；**不可由用户输入提供**（§5 安全 ✓）。
3. **单一装配点**：见 §3 前置 P0（今天链构造 **8 处异构** ✗）。

---

## 3. 前置条件（**阻断项**，必须先做）

### ✅ P0-a（**已撤销**，2026-10-16 复核）：`SourceFS` 的 raw 视图是**有意设计**，不是缺陷

**原假设（错）** ✗：`enclosure.SourceFS`（`pkg/volume/enclosure/attach.go`）对本地源卷返回
裸 `syncpkg.NewLocalFS(rt.AbsPath())` ⇒ 「绕过存储卷接口/封印」✗ ⇒ 要改为经 `RootFS` ✓。

**复核证据（推翻）** ✓：
- `SourceFS` 的代码注释**本就写明**「取源卷的**特权视图**（封印前语义：**直接 raw 视图，不受封印
  影响** —— 封装卷**必须能读写被封区间**；客户端则只能经封装卷由服务器处理）」，且「**LocalFS
  绕开封印正是「特权」的含义**」✓ —— 这是**声明式裁定**，非疏漏 ✓；
- **必须是 raw（非透明）**：封装卷（如 `secretdata`）**自行加密并写入密文**（分块/目录 meta 均
  密文形态）⇒ 换成 `pkg/rootfs` 的透明入口会 **双重加密** ✗（且读回无法解释）✓；
- **必须不受封印**：封装卷的全部读写都在源卷**被封区间**内 —— 「封装」正是「由 wrapper 代理
  访问该区间」⇒ 受封印则封装卷自身无法工作 ✗；客户端侧仍由 `storage.Root` 封印拦下 ✓；
- **能力面**：消费者依赖 `syncpkg.LocalVolume` 等可选能力（`secretdata.go:608` 断言 ✓）⇒
  换成仅实现核心 7 方法的适配器会**静默丢能力** ✗。

⇒ **`SourceFS` 不得改为 `rootfs.New(...)`** ✓；M2（关闭两个绕行点）**整体撤销** ✓。

**副产品（仍有价值）**：
- `Set.FSFor` **无生产调用点**（非测试源码仅本定义 ✓）⇒ raw 语义已在注释中显式标注为「内部
  布局/特权面，**用户内容面不得走**」✓，并列为 **M5 候选删除** ✓；
- **R26 白名单不需收缩**（两者均为有意 raw）✓ —— 原先「修完可收缩」的推论一并撤销 ✓；
- **能力面差异已取证**（`LocalFS` 实现 14 个可选能力接口中的多个，而 Root 适配器仅核心 7 方法 ✓）
  ⇒ 任何「把 raw 视图换成透明适配器」的改动**必须先逐项核对消费者** ✓。

### 🔴 P0-b：`storage.Root` 直操作路径必须划界

raw `root.OpenFile`（meta sidecar / s3 part / 块写暂存 / 索引快照 ✓）**不经过**存储卷接口 ✗
⇒ 要么纳入配额层 ✓、要么**显式豁免并登记** ✓（禁静默 ✓）。R25/R26 门禁从「注释纪律」升为**承重** ✓。

### 🔴 P0-c：链构造收敛

今天 **8 处异构**构造（`Guard(Wrap(raw))` / `PrefixFS(NewRootFS(tnt.Root()),"user/")` /
`Guard(NewRootFS(...))`（**无 Wrap** ✗）/ 备份 manifest 桶 … ✓，见 `backup_api` / `dav` /
`external_source` / `cloud_download_handler` / `files_service` ✓）⇒
「创建时自动包一层」**要求先收敛到一处** ✓（纯重构、行为不变 ✓，可独立验证与回退 ✓）。

---

## 4. 接口变更清单（**允许破坏性**，按用户裁定：不做兼容附加层）

| # | 变更 | 说明 |
|---|---|---|
| 1 | `volume.Volume` 增 `Kind()` | §1.2 ✓ |
| 2 | 存储卷增 `Footprint(rel)` / `MaxPhysical(logical)` | §1.3 ✓（每类卷一套实现 ✓） |
| 3 | `sync.FS.WriteFile` **改签名**：`(ctx, key LogicalKey, r, size, mtime) (written int64, err error)` | 返回**实际写入的物理字节** ✓；`LogicalKey{Owner, Bucket, Rel}` ✓。影响 **82 个调用点** ✗ ⇒ 一次性改（不保留旧签名 ✓，避免双形态 ✗） |
| 4 | `Rename`/`Delete` 同样携带 `LogicalKey` | 释放侧与结算侧**同源** ✓（消灭漂移类 ② ✓） |
| 5 | 删除 `files.Service.settleSize`/`physChargeFor`/`prevCharge`/`chargedHint` | 职责上移到配额层 ✓ |
| 6 | 配额查询：按卷联邦 | §1.5 ✓ |
| 7 | `charged_bytes` 记录落在**配额层** | S1 的最终归属 ✓（它同时持有写与删 ✓） |

**为何改签名而非加能力接口**：能力接口会保留**双形态**（老路径仍可忘 ✗）⇒ 复杂度上升且不封闭 ✗。
改签名后**编译器**强制每个写点传 `LogicalKey` ✓ ⇒ **义务变为类型可见** ✓（本设计的核心收益 ✓）。

---

## 5. 安全性审查（4 个新面）

| # | 风险 | 要求 |
|---|---|---|
| 1 | **归属可伪造** ✗：`LogicalKey` 若来自用户输入 ⇒ 跨租户配额操纵（把写入记到他人头上 ✓） | **服务端派生**：协议层从已认证身份构造 ✓；`LogicalKey` 不得直通请求体 ✓ |
| 2 | **无法归属 ⇒ 免费存储** ✗ | **fail-closed**（拒绝）+ 可观测；内部写走**豁免登记表** ✓ |
| 3 | **覆盖面必须封闭** ✗ | 门禁断言：① 每个 `KindStorage` 卷**必须有**配额层；② 每个写点**必须**传 `LogicalKey`；③ Wrapper **不得**注册配额层 |
| 4 | **次序**：配额层相对 `Seal/Guard` 的位置 ✗ | 必须在 **Guard/Seal 之下** ⇒ **先拒后记** ✓（封印写恒拒 ⇒ 不入账 ✓） |
| 5 | **豁免表是攻击面** ✗ | 豁免项**登记 + 审计 + metric**；新增豁免需评审 ✓（与 R25 白名单同型 ✓） |
| 6 | **Doxxing 配额**：联邦查询只返回**该用户可访问的卷** ✓ | 复用 `AllowedVolumes(owner)` ✓（已有 ACL 收敛 ✓） |

---

## 6. 边界场景

| 场景 | 处置 |
|---|---|
| **写半途失败**（写 4KB 后 err ✓） | 🔴 今天**漏账** ✗（`res.Release()` 后部分字节在盘 ✓）⇒ 配额层**按实测结算**（失败也结算 ✓） |
| **硬链接**（`PutFromStagedPath` 用 hard link ✓） | 🔴 按路径统计会**双计** ✗ ⇒ 需 inode/记录去重 ✓ |
| **去重 / 内容寻址**（同 blob 多逻辑文件 ✓） | 🔴 **双计** ✗ ⇒ 需引用计数（`Meta.RefCount` 已预留、**未接线** ✗）⇒ 本设计**不解决**，记档 ✓ |
| **staging → finalize 是 rename** ✗ | 🔴 **设计洞**：配额层只看到 rename ✓ ⇒ 必须支持「**rename 带记账**」✓ 或 finalize 改为写 ✓ |
| **跨进程 / 多节点** | 🔴 账本必须**持久**（per-volume ✓，`capacity.StorageManager` 为先例 ✓）否则不能宣称 G1 ✓ |
| 覆盖写（old > new ✓） | ✅ `prev` 同源（存储卷 `Stat` ✓） |
| **跨卷 move** | ✅ **两侧天然对称** ✓（本设计最大收益 ✓） |
| 整卷封装 | ✅ 不擦除 ✓（既有裁定 ✓） |
| 外部卷 | ⚠️ 无「本地 root」⇒ `Footprint` = 远端 stat / 后端自管 ✓（`ReserveSpace`/`StagingQuotaCapable` 已存在 ✓） |
| legacy 无记录对象 | ✅ 回落 `Footprint` + metric ✓ |
| 无配额部署（未配 `vol_capacity` / 无 owner 上限 ✓） | ✅ no-op（零回归 ✓） |
| **30s 等待语义** | 🟠 **行为变更**：调用点原 fail-fast 507，下移后可能**等待** ⇒ 逐路径显式决定 ✓ |
| 块写 / 部分写（`WriteAt` ✓） | ⚠️ 增量需 prev+new ✓ |
| sparse / zero-tail | ⚠️ 逻辑≠物理的又一来源 ✓（会话已有 zero-tail 启发式 ✓） |
| 封印区间 | ✅ 写恒拒 ⇒ 不入账 ✓ |

---

## 7. 明确**不覆盖**（划界，防过度承诺）

| 类别 | 原因 | 归属 |
|---|---|---|
| 云任务生命周期（create 预留 → 完成/取消结算 ✓） | 生命周期驱动，**非写驱动** ✗ | 保持独立账本 ✓ |
| probe（预留即释放 ✓） | 不是写 ✗ | 保持 ✓ |
| staging / S3 in-flight part | 设计已定「属运维余量」✓ | 保持独立 ✓ |
| syncmgr 占位/对账（`syncReservePlaceholder`/delta ✓） | 非逐写 ✗ | 保持 ✓ |
| 去重 / 引用计数 | 语义未定 ✓ | 独立排期 ✓ |

⇒ **本设计的真实收益 = 覆盖「内容写入」这一大类**（漂移缺陷的**全部**来源 ✓）；
把方案宣传成「统一全部配额」是**不诚实**的 ✗。

---

## 8. 决议表（**已裁定**，2026-10-16 用户「按建议处理」）

> 状态：**7 项全部按建议值裁定** ✓ ⇒ 实施时**不再逐项请示** ✓；若实施中发现某条**前提不成立**
> （本会话已有先例：P0-a 被证伪 ✗），则**停下来出证据 + 提修正案** ✓，不得静默偏离 ✗。

| # | 决议 | **裁定值**（= 建议值 ✓） | 裁定理由 |
|---|---|---|---|
| 1 | 归属通道形态 | `LogicalKey` **参数**（非 context ✓） | 与既有「显式传递优于 context 隐式」一致 ✓；context 隐式会让**漏传=静默错归属** ✗（本会话反复出现的缺陷类 ✓） |
| 2 | 上界来源 | 卷自报 `MaxPhysical`（唯一可行 ✓） | 没有一种卷的物理尺寸能由调用方推出 ✗（加密=公式、外部=远端 stat ✓） |
| 3 | 豁免清单 | staging / S3 part / meta sidecar / 索引快照 / probe / 云任务 —— **逐项签字** ✓ | 这些**不是用户内容**（或已由各自机制记账 ✓）⇒ 重复记账 = 双计 ✗；**每项须在实现 PR 里给出「为何豁免」的一句证据** ✓ |
| 4 | 配额层次序 | 在 `Wrap/Seal/Guard` **之下**（先拒后记 ✓） | 拒绝必须发生在**任何字节落盘之前** ✓；在装饰器之上会让封印/变换**先执行再拒绝** ✗ |
| 5 | 失败语义 | 半途失败**按实测结算** ✓（非释放 ✗） | 已落盘字节是**真实占用** ✓（释放会让账本小于磁盘 ⇒ 伪 507 类 ✗） |
| 6 | 跨进程 | 要求 **per-volume 持久账本** ✓（否则 G1 不可宣称 ✗） | 进程内树 ⇒ 跨进程盲区 ✗；账本**随卷走** ✓（卷删除/迁移结构性正确 ✓） |
| 7 | 迁移方式 | 严格「**先删上层、再加底层**」✓ | 并行期 = 双计 ✗（已实证 `committed += 2×size` ✗）；⇒ **禁跨片并存** ✓，每片**同片内删旧加新** ✓ |

---

## 9. 分片与验收矩阵

> **进展（2026-10-16）**：片 1（`Kind()` ✓ M0/M1a/M1b）、片 2（本地卷自报足迹/上界 ✓）**已完成**；
> 片 3 已拆为 **3a（整卷权威统一，已完成）** + **3b（5 个装配点收敛，待做）**——见 §9.1。

| 片 | 内容 | 验收（每片独立可回退 ✓） |
|---|---|---|
| **0** | 链构造**收敛一处**（纯重构 ✓） | 全量绿 + e2e 行为不变 ✓ |
| **1** | `Kind()` 显式化 + 门禁（Wrapper 无配额层 / Storage 必有 ✓） | 单测 + 门禁变异 ✓ |
| **2** | `Footprint`/`MaxPhysical` 落到各卷（加密公式 / secretdata 计划 / 外部 stat ✓） | 每卷**磁盘真值**逐字节断言 ✓（沿用 `EncryptedPhysicalSize` 的既有测试形态 ✓） |
| **3** | `QuotaVolume` 接管**整卷**配额（合并现有 5 个装饰器 ✓，**去双预留** ✗） | 对账矩阵 + 「无 `2×size`」断言 ✓ |
| **4** | `LogicalKey` 通道 + `sync.FS` 签名变更（**82 调用点** ✗）+ `SourceFS` 改造（P0-a ✓） | 4 个历史缺陷的**回归用例** ✓ + 矩阵 ✓ |
| **5** | `QuotaVolume` 接管**用户**配额 + 联邦查询端点 | 跨卷求和一致性 ✓ + ACL 收敛（只返回可访问卷 ✓） |
| **6** | `charged_bytes` 落该层 + **删除**上层启发式（`prevCharge`/`chargedHint` ✓） | 「释放 = 结算」不变式 ✓ |
| **7** | per-volume **持久账本** + 对账自校验 | 崩溃/重启/跨进程 ✓ |

---

### 9.1 片 3 具体计划（2026-10-16 取证）

**现状取证（重要）**：「整卷容量记账」今天散在 **5 个装配点**各自 `capacity.WrapBackend(be, counter)`：

| # | 位置 | 形态 |
|---|---|---|
| 1 | `pkg/server/volumes.go:317`（配置外部卷） | `NewPoolCounterPersistent` / `NewPoolCounter`（池同源 ✓） |
| 2 | `pkg/server/user_volume_api.go:280`（用户建卷） | `NewCounter`（持久化文件 ✓） |
| 3 | `pkg/volume/enclosure/attach.go:151,154`（封装卷） | 池同源 ✓ |
| 4 | `cmd/sproxy/secret_register.go:734`（secretdata） | 池同源 ✓ |
| 5 | `cmd/sproxy/user_volume_restore.go:173`（恢复） | `NewCounter`（持久化 ✓） |

⇒ 这正是 §0 根因的**标准形态** ✗：记账是**横切关注点**，却实现成 **5 个调用点各自的义务**——
**漏一处即「静默无限额」**（新卷型/新装配路径不会报错 ✗）。

**🔴 关键约束（实现前必须知道，否则会撞墙）**：`pkg/volume/capacity` **已 import**
`pkg/volume/registry`（用 `registry.ExternalBackend` ✓）⇒ `registry` **不能**反向 import `capacity`
（成环 ✗）⇒ `QuotaVolume` **不能**直接调用 `capacity.Wrap` ✗。

**解决方案（片 3a ✓ 已落地）**：在 **`registry` 侧定义消费者最小接口** `VolumeCounter`
（`TryAdd/Release/Used/Capacity` ✓）——Go 接口是**结构性**的 ⇒ `capacity.Counter` 的两种实现
（`PoolCounter` ✓ / `VolumeCapacityCounter` ✓）**天然满足**，**零 import** ✓。⇒ 配额层因此
**无需知道**整卷账本的实现形态（内存池 / 持久文件 ✓）。

**片 3b（待做，机械收敛）**：把 5 处的 `capacity.WrapBackend` 改为**构造 `QuotaVolume` 并注入 counter**
（**同片删除旧包装** ✗，否则双账本 ✗）；验收：① 既有容量用例（`capacity_fs_test` / `volume_quota_test`
/ `metrics_capacity_test` ✓）全绿；② **新增「恰好一次」断言**（同一卷写入 N 字节 ⇒ `Used == N`，
**非 2N** ✗）；③ 对账矩阵 ✓。

**为何先做 3a 而不直接改 5 处**：3a 让「整卷权威单一」**可表达** ✓，并把 3b 降为**纯注入点替换** ✓
（风险从「5 处语义变更」降为「5 处参数替换」✓）；且 3a 的「恰好一次」测试是 3b 的**安全网** ✓。

### 9.2 片 5 取证：per-owner 配额预警**静默 no-op** 缺陷 + **结论更正**（2026-10-16）

> ⚠️ **本节后半部分的「API 缺口」结论已于同日更正为错误** ✗ —— 正确结论见下方「更正」段。
> 保留原文是为了留存「**从调用点断言 API 语义而不看数据模型**」这个错误路径 ✓。

**🔴 缺陷（真实、已定位，未修）**：`pkg/server/routes_setup.go:113` 的**配额预警读取器**：

```go
h.alertEngine.SetQuotaUsageReader(func(owner string) (used, capVal int64) {
	sc := h.quotaScopeFor(owner, "")
	if sc == nil { return 0, 0 }
	return sc.Usage(), sc.MaxBytes()
})
```

`quotaScopeFor(owner, rel)` 首行即：`segs := strings.Split(rel, "/")` ⇒ 当 `rel == ""` 时
`segs[0] == ""` ⇒ **恒返回 nil** ✗ ⇒ 读取器**永远拿到 (0,0)** ⇒ **per-owner 配额预警静默失效** ✗
（无日志、无指标、无报错 ✓ —— 「生效状态不可观测」✗）。且 `rel == ""` 的调用点**全仓仅此一处** ✓
（`grep 'quotaScopeFor([^,]*, "")'`）⇒ 非通用陷阱，而是**此处用错了 API** ✓。

**🔴 为何不能简单改成「owner 根 scope」** ✗：owner 根由
`ensureTenantQuotaLocked` 首返回值给出（`h.globalPool.Scope("/tenant/"+owner, quotaBytes)` ✓），
但 **`quota.Scope.Usage()` 返回的是 `s.pool.Usage()`**（**整个池**的用量 ✗，非该节点的 ✗）⇒
直接用会报**全局**用量 ⇒ 所有 owner 的预警**互相干扰** ✗（正是 `stagingQuotaPool` 注释里
警告过的跨租户干扰形态 ✗）。

**⇒ 真正的缺口** ✓：`quota.Scope` **没有 per-node（子树）用量访问器** ✗ —— `Usage()`/`Reserved()`/
`MaxBytes()`/`Available()`/`UsageByBucket()` **全部委托给池** ✗。⇒ 「owner 用量」目前**无法从
现有 API 读出** ✗ ⇒ 这不是拼写错，而是 **API 缺口** ✓。

**归属** ✓：该缺口正是 **片 6（`charged_bytes` 记录化）**要补的能力 ✓（「结算 = 释放同源」
的记录化必然要求在**节点**上记录已确认占用 ⇒ 顺带提供 per-node 用量 ✓）。
**在片 6 落地前不得**用「全局用量冒充 owner 用量」✗（那是把静默 no-op 换成静默**误报** ✗，更坏）。
⇒ 本项**显式挂起**并在此存档，防后人重踩 ✓。

### 9.3 片 6 取证：释放侧 **~20 处各自计算** vs 结算侧 **单源**（2026-10-16）

**现状（实测）** ✓：**结算侧已单源**（`Service.settleSize` = 卷自报足迹优先 ⇒ `UploadRoute.PhysicalSize` ⇒ 恒等 ✓）；
但**释放侧仍散在 ~20 处各自计算** ✗：

| 来源族 | 代表站点 | 依据 |
|---|---|---|
| **① 公式折算** | `trash.go:280,287`（`physicalCharge(root,size)` ✓）、`write_ops.go:935` | `RootPhysSizer` ⇒ `storage.EncryptedPhysicalSize`（**仅加密卷** ✓） |
| **② `Stat` 已物理** | `version_store.go:276`、`write_ops.go:971/997/1516`、`dav_meta.go:94` | 加密卷上 `Stat` 已是密文长度 ✓（注释自证「不再折算」✓） |
| **③ 删除前捕获** | `write_ops.go:1676`（`DeleteFileInput.chargedHint` ✓）、`prevCharge`（覆盖写旧值 ✓） | 原路径已消失 ⇒ **必须在改名/删除前捕获** ✓ |
| **④ 其它** | `chunked_upload.go:1420`、`dir_charge.go:134`、`cloud_archive_handler.go:742`、`files_service.go:1096`、`write_ops.go:1291/1854/2080` | 各异 |

**⚠️ 该漂移候选已于同日【证伪】✗✓**（片 6 第 1 步实测）：外部/封装卷的软删被**显式拒绝**
（`?soft=true` ⇒ 400「外部卷暂不支持回收站删除：请改用硬删」，`pkg/files/write_ops.go:1772`
的 fail-closed 分支，注释自证「禁静默降级」）⇒ **回收站释放路径只作用于本地卷** ✗；
而本地卷的**结算侧同样走公式**（`RootPhysSizer`，无自报能力）⇒ **两族来源永不重叠** ✓ ⇒
`释放 == 结算` 成立 ✓。硬删路径走 `DeleteFileInput.chargedHint`/`prevCharge`（本就**卷自报
足迹优先** ✓）⇒ 同源 ✓。**回归用例**：`TestWrapperSoftDelete_RefusedFailClosedAndNoSideEffect`
（钉住「拒绝 + **零副作用**」）✓。⇒ 片 6 的「释放 ≠ 结算」**不需要**~20 站重构 ✗。

（以下为**原始嫌疑记录**，保留以留存该判断路径。）

**🔴 漂移候选（**尚未用测试证实** ✗ —— 按本会话纪律不得无证据断言 ✓）**：对 **secretdata** 这类
「物理量含**公式不可推**的部分」的卷（meta blob pad 到 `[base, 2·base)`、XOR parity 可达逻辑 2× ✓）：
**结算**走卷自报足迹（`VolumePhysFootprinter.PhysFootprint` ✓，含 meta pad ✓），而**回收站释放**
走 `physicalCharge`（`EncryptedPhysicalSize` 公式 ✗，**覆盖不到 meta pad** ✗）⇒ 理论上
**少释放 = 幽灵占用 / 假 507** ✗。
**待验证** ✓：需构造「secretdata + 软删 + owner 配额」用例，断言 `结算额 == 释放额` ✓
（**片 6 的第一件事就是这个回归用例** ✓，先红再修 ✗）。

**片 6 交付形状** ✓（按决议 #7：**同片内删旧加新** ✗）：
1. **写时记录** `charged_bytes`（落对象 meta `Extra` ✓ —— `FileMeta.Extra map[string]any` ✓ +
   `sync.MetaExtraUpdater.UpdateMetaExtra` 旁路写入 ✓，已有先例：`cloud/transfer.go:412/528` ✓）；
2. **释放侧唯一入口** `releaseChargeFor(route, owner, remotePath, fallback)`：
   ① 已记录 ⇒ 读记录（**同源** ✓）；② 对象仍在 ⇒ 卷自报足迹 ✓（= `prevCharge` 现有逻辑 ✓）；
   ③ 对象已消失（改名/删除后）⇒ **必须在操作前捕获** ✓（`chargedHint` 模式 ✓）；
3. **逐站迁移** ~20 处到该入口（**同片删旧计算** ✗）；
4. **不变式测试** ✓：`结算额 == 释放额`（对每个卷型 ✓，含 secretdata ✓）+ 变异命中。

**为何不本轮做** ✓：~20 站迁移 + 记录机制 + 每卷型回归 = 体量与片 4 同量级 ✗；
且**第 1 步（回归用例）必须先红** ✗ —— 否则可能是在修一个**不存在**的漂移 ✗（本会话已有 P0-a 先例 ✓）。

### 9.4 片 4 前提复核：`LogicalKey` **对当前架构不是必需**（2026-10-16 取证）

**实测（三条）** ✓：
1. **`LogicalKey` 全仓零命中**（`grep -rn LogicalKey --include=*.go pkg/ cmd/` ⇒ 无输出 ✗）
   ⇒ 它是**纯设计概念**，从未实现 ✓；
2. **`QuotaVolume` 零生产调用点**（`grep -rn 'QuotaVolume|\.Reserve\(|VolumeUsed\('` 除
   `pkg/volume/registry` 自身外 ⇒ 无输出 ✗）⇒ M4a/M4b 的配额层**完全未接线** ✓；
3. **归属解析发生在封装卷【之上】** ✓：`Service.settleSize(route UploadRoute, owner, remotePath string,
   written int64)` 由**写内核**传入 `owner`/`remotePath` ✓ ⇒ 不依赖卷内路径可归属 ✗。

**⇒ 结论** ✓：`LogicalKey`（把归属**下传**到卷内）只服务**本设计的§1.1 目标分层**（配额层包在
**存储卷**上 ⇒ 到达它时路径已是封装卷内部名 ✗）—— **不是**当前架构的正确性所需 ✓。
⇒ **片 4（`sync.FS.WriteFile` 签名变更 + 82 调用点）在「消灭缺陷」维度上收益为 0** ✗
（本会话已分别验证：整卷侧已单源 ✓（片 3b 唯一入口）、每用户侧已单源 ✓（片 6 证伪漂移）、
四类历史漂移均已修复或证伪 ✓）。
⇒ **它的真实价值 = 架构一致性**（让「配额权威在卷」成立 ✓）⇒ **属于重构/迁移，不属于修复** ✗；
在未证明「当前双位置（写内核 + 卷容量装饰器）会造成**具体**缺陷」之前，**不建议**做 ✗
（代价：82 调用点 + 每卷型回归 = 全库最大一片 ✗）。

**待用户裁定** ✓：① 片 4 **缓做/不做**（本文档倾向「缓做」✗）；② `QuotaVolume`（M4a/M4b，
当前零调用点 ✗）是**接线**（找一个真实消费者）还是**回收**（避免死代码 ✗）——两者均需先定
「配额权威是否真要进卷」这个**方向性**裁定 ✓。

**本会话已完成的真实收益（与片 4 无关）** ✓：M4a（双账本骨架 + 覆盖写双计修复）、片 2（卷自报
足迹/上界）、片 3a/3b（整卷权威统一 + 唯一入口 + R29）、片 5（per-owner 预警静默 no-op 修复）、
片 6（漂移证伪 + 回收站边界不变式）、M5 步 1（死代码 `Set.FSFor` 删除）。

### 9.5 片 4a 决议：`LogicalKey` **形状** + **通道形态**（2026-10-16）

**取证** ✓：`sync.FS` 接口（`pkg/sync/entry.go:54`）：
```go
WriteFile(ctx context.Context, path string, r io.Reader, size int64, mt int64) error
```
非测试调用点 **120 处** ✗；`sync.FS` 实现 **15+**（`LocalFS`/`PrefixFS`/`EncryptedFS`/
`StagingQuotaGateFS`/`rootFSCore`/`remoteFS`/`subPathFS`/`CapacityFS`/`backupQuotaFS`/
`davMetaFS`/`rootSecretsFS`/`enclosure`/`secretdata`/外部卷族 ✓）。

#### 🔑 形状：`LogicalKey{Owner, Rel string}` —— **只有 `owner` 是丢失的信息**
**关键洞察** ✓：路径参数 `path` **已经携带** `bucket/rel`（客户端可达路径恒 `<卷根>/<owner>/<bucket>/<rel>` ✓）；
封装卷内部名（随机容器 + 内容哈希）丢掉的**只是 `owner`** ✗ —— 但它同时**丢失了「原 `rel` 用于作用域
最长前缀解析」的能力** ✗（配额按功能桶/子目录最长前缀定 Scope ✓）⇒ 两者都要 ✓。
⇒ 即：`LogicalKey` = **写内核已有的 `(owner, remotePath)` 对** ✓（`Service.settleSize(route, owner,
remotePath, written)` ✓）—— 不是新概念，而是**把这个对从写内核下传到卷内** ✓。

**为何不用单一字符串键** ✗：字符串需重新解析（且“拼回去再拆开”是归属漂移的经典根因 ✗）；
结构化两字段让**非法状态不可表示** ✓（与仓内 `Location`/`Ref` 一致 ✓）。
**零值语义** ✓：`LogicalKey{}` = **无归属**（调用方不声明 ⇒ 配额层**显式不记账** ✗，**禁**猜 ✗）——
与「未判定不得当 Storage ✗」同型。

#### 📡 通道形态：**参数**（决议 #1）
```go
WriteFile(ctx context.Context, key sync.LogicalKey, path string, r io.Reader, size, mtime int64) error
```
**为何参数而非 context** ✗（决议 #1 已定）：context 隐式会让**漏传 = 静默错归属** ✗（本会话反复
出现的缺陷类 ✓）；参数使**编译器强制**每个调用点表态 ✓（要么传真归属 ✓、要么显式 `LogicalKey{}` ✓）。

#### 迁移序（4b，机械但量大）
1. **先加参数**（接口 + 15+ 实现 + 120 调用点，**编译器驱动** ✗）——调用点绝大多数传
   `LogicalKey{}`（它们**不在**封装卷下、`path` 已可归属 ✓）；
2. **再在封装卷下传**（secretdata 写入时传真实 `(owner, rel)` ✓）——**同一片内**（禁双形态 ✗）；
3. **配额层消费**（`QuotaVolume.Reserve(key, n)` 按 key 归属 ✓）；
4. **回归矩阵** ✓：4 类历史漂移（单位不一致 / 释放≠结算 / 归属丢失 / 静默降级）逐项用例 ✓。
**风险提示** ✓：步骤 1 纯机械（编译器强制 ✓），步骤 2 是**语义变更**（配额层开始记账 ⇒
**同片必须删上层重复记账** ✗，否则双计 ✗ —— 已实证 `committed += 2×size` ✗）。

#### 🔴 脚本化迁移的**强制守卫**（2026-10-17 事故记录）

首次尝试用 `os.walk(<repo root>)` 批量改 `WriteFile` 签名 ⇒ **误入 `.worktrees/`** ✗ ——
那里是**其他分支的独立工作树**（含未提交 WIP ✗）⇒ 一次改动了 **228 个文件**（跨 3 个 worktree ✗）。
**已用逆向正则外科式完整恢复** ✓（只撤销插入的 `key LogicalKey` 参数 ⇒ **不动任何无关 WIP** ✓）；
主树 `git checkout -- .` 恢复干净 ✓。

**⇒ 强制规则（后续任何仓级脚本化改动）** ✓：
1. **必须排除** `.worktrees/`（以及任何嵌套 worktree ✗）—— 推荐只遍历 `pkg/`、`cmd/`、`internal/`、
   `test/`、`web/` 这些**显式白名单**目录 ✓，**禁止**从仓库根 `walk` ✗；
2. 改动前先 `git worktree list` 确认边界 ✓；
3. 恢复优先用**逆向变换**（外科式 ✓）而不是 `checkout` ✗ —— `checkout` 会连带毁掉**无关 WIP** ✗；
4. 落盘前先 `git status --porcelain` 在**每个 worktree** 各查一次 ✓。

---

## 10. 风险与回退

- **最大风险**：迁移期**双账本双计** ✗ ⇒ 每片必须**同片内删旧加新** ✓（禁跨片并存 ✓）
- **回退**：片 0/1 纯结构 ✓；片 2 加法式 ✓；片 3+ 每片可 revert（旧装饰器删除与新增同片 ✓）
- **不可省的三条** ✓：① 归属显式下传（安全特性不可绕 ✗）；② 上界由卷自报（G1 唯一路径 ✓）；
  ③ 迁移严格先删后加 ✗

---

## 附：与既有设计的关系

- 本设计 = `2026-10-14-quota-model-v3.md` 的 **S1（记录化释放）+ S3（去双账本）+ S6（跨包门禁）** 的
  **结构性实现** ✓ —— 那些分片在此找到了**唯一正确归属**（配额层 ✓）。
- `2026-10-11-enclosure-volume.md` 的「封装卷配额继承源卷」✓ 由 §1.4 的「封装卷零配额代码」**取代** ✓。
- R25/R26 门禁从「白名单纪律」升级为「**接口保证**」✓（§3、§5）。

---

## 11. 统一存储卷模型与渐进迁移

### 11.1 现状：**数据面已统一，存储身份未统一** ✗

| 面 | 现状 | 证据 |
|---|---|---|
| **数据面（读写）** | ✅ **已统一** —— 本地与外部都经 `sync.FS` | `ExternalBackend` 仅 `FS() + Close()`（`registry/backend.go:21` ✓）；本地经 `NewRootFS`/`FSFor` ✓ |
| **存储身份** | 🔴 **未统一** —— `*storage.Root` 是非测试源码的**局部特例** | `storage.Root` 出现 **254 次** ✗；`ExternalBackend` 121 次 ✓ |
| **装配分派** | 🔴 **本地是特例** —— 不在 `backendFactories` 里 | `volume.go:26`「`Type == "" || TypeLocal` **走本地卷路径**；外部后端用各自字面量」✗ |
| **类型分类** | ⚠️ **部分存在** —— registry 已有 `backendCategory` 静态基线 + `wrapperTypes` 动态登记 | `registry/backend.go:127-131` ✓ |
| **能力探测** | ✅ **已成熟** —— 可选能力接口模式 | `Presigner` / `URLResolver` / `VolumeStatsProvider` / `StagingQuotaCapable` / `WriteIfAbsent` / `ReserveSpace` ✓ |

### 11.2 两个**绕行点**（必须先关，否则统一模型无意义）✗

| 位置 | 现状 | 后果 |
|---|---|---|
| ~~`Set.FSFor(name)`~~（**已删除**，2026-10-16） | 曾是本地 → **`LocalFS(RootDir)`** ✗；复核证实**零调用点**（连测试都没有 ✗，`grep '\.FSFor('` 无命中）⇒ **死代码** ⇒ 已删 ✓ | 原后果：绕过 `storage.Root` ⇒ 加密卷读密文/写明文 + 绕过封印 ✗ —— **随删除消失** ✓ |
| `enclosure.SourceFS`（`enclosure/attach.go:166`） | 本地源 → **`LocalFS(rt.AbsPath())`** ✗ | 同上，且**配额层看不到封装卷写入** ✗（§3 P0-a ✓） |

⇒ 二者同属 **R26「卷根直连适配器」** 族 ✓（历史 P0 全出自「绝对路径 + 裸 os」✗）。

### 11.3 目标模型：**必需核心 + 可选能力**（沿用既有哲学 ✓）

```go
// pkg/volume 新增：存储卷的**统一身份**（数据面 + 控制面 + 生命周期）
type StorageVolume interface {
    Name() string
    Kind() Kind              // §1.2：Storage | Wrapper（显式 ✓）
    FS() syncpkg.FS          // 数据面（**已经是**统一面 ✓）
    Close() error            // 生命周期（与 ExternalBackend 同形 ✓）
}

// 可选能力（类型断言，缺省即不支持——与 Presigner/ReserveSpace 同模式 ✓）
type Footprinter   interface{ Footprint(owner, rel string) (int64, bool) }  // 物理足迹（§1.3 ✓）
type PhysBound     interface{ MaxPhysical(owner, rel string, logical int64) int64 }
type CapacityOwner interface{ Capacity() int64; SetCapacity(int64) }        // 整卷配额 ✓
type StatsProvider interface{ VolumeStats() (used, total int64, ok bool) }
```

**要点**：
1. **`local` 成为普通后端** ✓ —— 注册 `backendFactories[volume.TypeLocal]` ✓，构造器返回 `StorageVolume`（包 `storage.Root` ✓）。
   装配层**删掉** `Type == "" || TypeLocal` 特例分支 ✓（空串归一为 `TypeLocal` 后统一分派 ✓）。
2. **`storage.Root` 成为 local 后端的内部实现细节** ✓ —— 不出现在接口上；254 处调用点逐步迁移 ✓。
3. **两阶段构造保留** ✓（`deferredBackends` ✓：secretdata/secrets 依赖卷集 ✓）。
4. **可选能力而非大接口** ✓ —— 外部后端只需 `FS()+Close()` ✓（今天已经如此 ✓，**零回归** ✓）。

### 11.4 渐进迁移（每片独立可验证 + 可回退 ✓）

| 片 | 内容 | 行为变更 | 验收 |
|---|---|---|---|
| **M0** | 定义 `StorageVolume`/`Kind`/可选能力接口（**加法式** ✓） | 无 ✓ | 编译期断言（各卷实现）✓ |
| **M1** | 注册 `local` 后端（返回 `StorageVolume` 包 root ✓）；装配层**先并存**（特例分支委派到新后端 ✓） | **无** ✓ | 全量绿 + e2e 不变 ✓ |
| **M2** | **关闭两个绕行点** ✓（`FSFor`/`SourceFS` → 返回卷的 FS ✓） | **有**（加密本地卷对封装卷/嵌套写**现在会正确加解密 + 封印生效** ✓ = 修缺陷 ✓） | 加密卷封装/嵌套 e2e + 封印 fail-closed ✓ |
| **M3** | `Set.Root()` 调用点迁移（254 处 → 收缩 ✓）；**棘轮门禁**（只减不增 ✓，同 R14/serial 预算 ✓） | 无 ✓ | 棘轮断言 + 逐包迁移 ✓ |
| **M4** | 配额层挂到**每个** `StorageVolume` ✓（§9 片 3–5 ✓） | 有（记账口径 ✓） | 对账矩阵 ✓ |
| **M5** | **删除**特例分支与 `Set.Root()` ✗ | 无 ✓ | 门禁 + 全量 ✓ |

**棘轮（关键防回退）** ✓：新增门禁断言「非测试源码中 `Set.Root(` / `storage.Root` 直接使用**不得增加**」✓ ——
既有 R26 白名单在此**同步收缩** ✓（M2 后 2 处 → 0 处 ✓）。

### 11.5 收益

1. **特例消失** ✓：本地/外部同形 ⇒ 新后端（新云盘/S3 兼容/集群卷 ✓）**只需实现 5 个方法** ✓。
2. **绕行点归零** ✓：加密/封印/配额对**所有**卷路径一致 ✓（消灭历史 P0 的整族 ✗）。
3. **配额层有了唯一挂载点** ✓（§1 ✓）——这正是 §3 P0-c（链构造收敛）的**结构性解法** ✓。
4. **可观测/可门禁** ✓：`Kind()` + 棘轮 ⇒ 「谁绕过卷模型」成为**编译/门禁可见** ✓。

### 11.6 与 §9 的关系

**M0–M2 是 §9 片 0–1 的具体化** ✓，且 **M2 同时完成 §3 的 P0-a** ✓；
**M3 的棘轮**是 §3 P0-b（raw `storage.Root` 划界）的**可执行版本** ✓。
⇒ 两套分片**可合并为一条线**：M0 → M1 → **M2（=P0-a）** → M3（=P0-b 棘轮） → M4（=§9 片 3–5） → M5。

---

## 勘误（2026-10-16 复核）：**M2 整体撤销**，P0-a 不成立

**结论** ✗：`SourceFS`（`pkg/volume/enclosure/attach.go`）与 `Set.FSFor`（`pkg/volume/registry/set.go`）
的**本地卷 raw 视图**都是**有意设计**，不是「绕过加解密/封印」的缺陷 —— 详见 §3 的 P0-a 条（已改为
「已撤销」并附四项证据 ✓）。**M2b / M2c 不再实施** ✗。

**为何这是重要修正** ✓：若按原计划把两处换成 `pkg/rootfs` 的透明适配器，会造成：
① **双重加密**（封装卷自行写密文 ✓）；② **封印特权访问失效**（封装卷必须能读写被封区间 ✓）；
③ **静默丢能力**（消费者依赖 `syncpkg.LocalVolume` 等 ✓）。

**仍然有效并保留的成果** ✓：
- **M2a**（本分支已落地 ✓）：适配器与用户可见读写入口下移到 `pkg/rootfs` —— 其价值独立于 M2 ✓
  （「用户内容读写唯一入口」现在可被**下层包**复用，且 R25 门禁白名单已同步 ✓）；
- **M1/M1b**（`local` 注册为普通后端 + 用户可创建面守卫 ✓）与 **M0**（`Kind`/`StorageVolume`/
  `registry.KindOf` ✓）**不受影响** ✓；
- 后续分片改为：**M3**（`Set.Root()` 254 处迁移 + 棘轮门禁 ✓；`FSFor` 列为**候选删除** ✓）→
  **M4**（配额层挂每个 `StorageVolume` ✓）→ **M5**（删除 `Type == "" || TypeLocal` 特例分支 ✓）。

**方法论** ✓：这是本会话第 **N** 次「**修前先验证前提**」救回错误改动 ✓ —— 而本次的证据**就在代码注释里**
（`SourceFS` 的 doc 已写明「特权视图 / raw 是特权含义」✓）⇒ **动一个「看起来像缺陷」的 raw 视图前，
必须先读它的 doc 与消费者** ✓（能力断言、双重变换风险、封印语义三条缺一不可 ✓）。

---

## 12. PR 范围与后续计划（2026-10-17 用户裁定）

### 本 PR（#814）交付范围 —— **完整功能 PR，可独立合并** ✓

| 片 | 内容 | 状态 |
|---|---|---|
| M0 | `volume.Kind` + 能力接口（`Footprinter`/`PhysBound`/`CapacityOwner`/`StatsProvider`）+ `registry.KindOf` | ✅ |
| M1a/M1b | `localBackend` 统一契约 + `local` 注册为普通后端（含 2 个 API 面守卫） | ✅ |
| M2a / M2 撤销 | `pkg/rootfs` 下移；**raw 视图前提被证伪** ⇒ 撤销 + 勘误 | ✅ |
| M3 | `Set.Root` 访问器 21→14 + **R28 棘轮门禁** | ✅ |
| M4a | `QuotaVolume` 双账本骨架（**覆盖写双计**真缺陷修复） | ✅ |
| 片 2 | 本地卷自报**物理足迹/写前上界**（加密卷 = 密文长度） | ✅ |
| 片 3a/3b | 整卷权威统一（`VolumeCounter`）+ **唯一入口** `WrapStorageVolume` + **R29 门禁**（含**能力静默丢失**自伤修复） | ✅ |
| 片 5 | **per-owner 配额预警静默 no-op 修复**（`ownerQuotaScope`） | ✅ |
| 片 6 步 1 | 「释放 ≠ 结算」漂移嫌疑**证伪** + 回收站边界不变式（拒绝 + 零副作用） | ✅ |
| M5 步 1 | 删死代码 `Set.FSFor` + R26 白名单同步 | ✅ |
| 接线 | `QuotaVolume` 可达 + 卷用量**单一查询面**（收敛两处重复逻辑） | ✅ |
| 片 4a / 4b 步① | `LogicalKey{Owner,Rel}` 形状决议 + 类型 + 配额层 API 采用（零值显式不记账） | ✅ |

### 后续 PR（**不在本 PR**，用户裁定 2026-10-17）

1. **原方案迁移** ✓：`sync.FS.WriteFile` 签名变更（+`key LogicalKey` 参数，全仓破坏性）
   —— **单独 PR** ✓。本 PR 已完成其**前置**（`LogicalKey` 类型 + 配额层消费侧 API ✓）；
   迁移本身需**逐文件手工**（实测：~22 非测试调用点 + ~56 实现签名 + ~30 测试 mock；
   四类边界：未命名参数 mock ✗ / 同名不同接口 ✗ / 多行调用 ✗ / 子包限定符 ✗）。
2. **片 6 完整**：`charged_bytes` 记录化 + 释放侧唯一入口 + ~20 站迁移（**同片删旧计算**）。
   - **实际落地方式（2026-10-18 定稿，与设想不同但目标一致）**：未引入「记录值」存储（需新
     ledger + 迁移 + 陈旧风险），改为**同源捕获**——释放金额一律取**卷自报实测足迹**
     （`physChargeFor` / 装配层 `ReleaseChargeFor`），并要求调用方在**删除/改名之前**捕获
     （对象存在 ⇒ 足迹可测）。已覆盖 **24 处**调用点（写/删/改/版本/回收站/回收清理/覆盖写）。
     **关键洞察**：软删条目**仍存在于 trash 路径** ⇒ 可对**其自身路径**取足迹（`PhysFootprint`
     按 rel 查索引、改名后索引随迁）⇒ 无需记录化即可同源。
     **残余**：`charged_bytes` 记录化对「对象已不可测且**不在** trash 索引」的场景仍有价值（当前无
     此类场景）⇒ 留待出现真实需求再评估。
3. **M5 剩余**：装配层 `Type=="" || TypeLocal` 特例合并（本地卷经 `registry.NewBackend` 统一分派）。
4. **片 5 剩余**：per-user 配额**按卷持有** + 联邦查询端点（含 Web UI ⇒ Playwright e2e）。
   - **已交付（读侧，PR 见 git）**：`QuotaVolume.UserUsage/UserCapacity`（**单卷**每用户读侧 ✓）
     + `Handlers.federatedUserQuota`（**唯一**汇总入口：只遍历 `AllowedVolumes` ⇒ ACL 收敛 ✓；
     无权威卷 `HasQuota=false` + 整体 `Partial=true` ⇒ **未知≠0** ✓；上限 0 = 不限 ⇒ `Unlimited` ✓）
     + `GET /api/quota`（owner **恒取自认证** ✓，不接受请求参数 ✗）。
   - **仍待**：写侧**按卷持有**（= 片 4 步骤③ / 片 6，与「删上层重复记账」同片 ✗）+ Web UI 呈现
     （改 UI 须补 Playwright e2e ✗）+ 跨进程/持久账本（片 7 ✓）。

---

## 13. 写侧统一：`QuotaVolume` 作为**唯一记账权威**（目标架构，2026-10-17 定稿）

> 用户指示（2026-10-17）：「**注意选最佳形式组织架构，无需考虑兼容**」⇒ 本节给出**目标形态**，
> 实施时**不留兼容层 / 不并存双形态**（§10 的「迁移严格先删后加」✓）。

### 13.1 现状取证（**实测**，非估计）

| 项 | 实测 | 说明 |
|---|---|---|
| `route.Release()` 调用点 | **52** | 失败路径双回滚 |
| `UploadRoute.Scope` / `ScopeRes` / `Pool` / `PoolRes` / `PhysSize` | 各 **27–28** | 记账字段消费点 |
| 测试引用（`ScopeRes`/`PoolRes`/`PhysSize`/`UploadRoute{`） | **50** | mock 与断言 |
| `settleSize` 调用点 | **6**（非测试） | 写内核结算 |
| `CapacityFS` 内池预占点 | **7** | 🔴 **前提已被证伪**（2026-10-17 实证）：**不是**重复预留 ✗ —— 外部/封装卷的池预留**立即 `Release()`**（注释：「仅探测用于路由/快速失败，立即释放防双计」✓）；**本地卷**才由 `UploadRoute.Pool` 结算（本地卷不包 `CapacityFS`）⇒ 真缺陷是**「整卷记账权威」按卷类型分治**（同一概念两个权威 ✗） |
| `chunked_store.go` 会话级预留 | `Reservation` / `PoolRes` / `StorageMgrReserved` **三套** | 分块上传独立面 |

**结论**：这是**跨多会话的原子重构** ⇒ 必须一次成型（禁半成品 ✗）。

### 13.1b 🔴 **重新裁定：§13 的收益被高估，不建议实施**（2026-10-17 第 11 次前提证伪）

起草 §13 时我列出两条「最佳形式」的对比理由 ✗ —— 逐条**实证后均不成立**：

| §13.3 的原始主张 ✗ | 实证结论 ✓ |
|---|---|
| 「物理尺寸**公式散在调用方**」✗ | **已是最佳形态**：`settleSize` **首选卷自报** `VolumePhysFootprinter.PhysFootprint` ✓（装配层向**卷**询问 ✓），`UploadRoute.PhysSize` 只是**能力缺失时的回落** ✓ ⇒ 不存在「公式外泄」✗ |
| 「整卷权威**按卷类型分治**」= 缺陷 ✗ | **结构性必然** ✓：本地卷写路径走 `Tenant.Root()`（`*storage.Root` ✓），**外部卷**走 `FS` ✓ ⇒ `CapacityFS`（包 `FS` 的装饰器）**结构上无法**覆盖本地卷 ✗ ⇒ 本地必须由 `UploadRoute.Pool` 记账 ✓。**不是疏漏** ✗ |
| 「双账本靠调用点自律」✗ | 已有**单一事实源** ✓：`UploadRoute.Commit`（`service.go`，注释明写「本方法是该结算规则的**单一事实源**」✓），装配层 `*volumeRoute` 已**不再持有** commit 方法 ✓ |

**⇒ 结论** ✓：§13 的改造是**结构性偏好**（把记账字段搬进配额层），**不是缺陷修复** ✗；
按本仓既定判据（「**设计文档说要改 N 处不是实施理由**，必须先答『不改造成什么**具体缺陷**』」
—— 见 §12 末），**答不上来** ✗ ⇒ **不建议实施** ✗。

**若仍要做**（用户可裁定 ✓），必须先解决一个**前置设计问题** ✗：让本地卷也具备统一配额权威，
要么 ① 把本地写路径改经 `FS`（触及 `Tenant.Root()` 全链 ✗），要么 ② 在 `QuotaVolume` 中为
`*storage.Root` 形态增加第二套适配（等于**保留**分治 ✗）—— 两者都不是「零成本搬家」✗。

**真正的遗留项**（有具体缺陷 ✓，见 §12 队列）：
- **片 7 持久账本**：修「**跨进程配额不可见**」✓（多节点各自记账 ⇒ 可超限 ✓）—— 这才是**具体缺陷** ✓；
- **片 6 完整**：`charged_bytes` 记录化 + 释放侧唯一入口（消灭「释放 ≠ 结算」漂移的**残留启发式** ✓）。

---

### 13.2 目标形态（**单一记账权威**）

#### 🔴 分层约束（**2026-10-17 实证，推翻本节初稿**）

初稿写 `UploadRoute{Quota *registry.QuotaVolume}` ✗ —— **架构非法**：`internal/archcheck/layers.go`
把 `pkg/files` 定为 **L1**、`pkg/volume/registry` 定为 **L2**，且该文件注释明写：

> 「卷集合与容量核算经领域自定义窄接口（`files.VolumeSet` / `files.StorageManager`）由装配层注入
> （见 `pkg/files/service.go`）。**此处若被"补回"子包依赖，门禁 R2 会报红**。」

⇒ `pkg/files` **不得** import `pkg/volume/registry` ✗（由 **R2 门禁**强制 ✓）。
⇒ 正确形态 = 仓库**既定模式**：**领域侧拥有窄接口 + 装配层适配**（依赖倒置 ✓）。

#### 目标形态（**依赖倒置**）

```go
// pkg/files —— 领域侧**自己拥有**的窄接口（只含写内核真正需要的三个动作）。
// 装配层以 *registry.QuotaVolume 适配（pkg/server，L2 之上，可 import registry ✓）。
type QuotaAuthority interface {
    Reserve(key syncpkg.LogicalKey, upperBound int64) (QuotaReservation, error)
    Footprint(owner, rel string) (int64, bool)   // **卷自报**物理足迹（释放侧同源）
    MaxPhysical(owner, rel string, logical int64) int64 // 写前上界（**公式只写在卷里**）
}

// QuotaReservation 是**双账本预留句柄**的领域视图（一次 Reserve 覆盖两侧）。
type QuotaReservation interface {
    Commit(prev, written int64) // 结算（覆盖写差分；两侧各自幂等）
    Release()                   // 双回滚
}

// pkg/files —— 路由只带「去哪写」+「一本账句柄」；记账细节全部内聚在权威实现里。
type UploadRoute struct {
    VolumeName string
    Tenant     *storage.Tenant    // 本地卷写面
    Sink       UploadSink         // 外部卷写面（非 nil 时优先）
    Key        syncpkg.LogicalKey // 归属键（真实 (owner, bucket/rel)）
    Quota      QuotaAuthority     // 该卷的配额权威（nil = 该卷无配额层 ⇒ 不记账）
    Res        QuotaReservation   // 预留句柄（nil = 未预留）
}
```

**适配层**（`pkg/server`，本片新增 `filesQuotaAdapter`）：

```go
type quotaAuthorityAdapter struct{ q *registry.QuotaVolume }
func (a quotaAuthorityAdapter) Reserve(k syncpkg.LogicalKey, n int64) (files.QuotaReservation, error) {
    r, err := a.q.Reserve(k, n)
    if err != nil || r == nil { return nil, err }
    return r, nil // *registry.Reservation 结构性满足 files.QuotaReservation（两方法同名同形）
}
func (a quotaAuthorityAdapter) Footprint(owner, rel string) (int64, bool) { return a.q.Footprint(owner, rel) }
func (a quotaAuthorityAdapter) MaxPhysical(o, r string, n int64) int64    { return a.q.MaxPhysical(o, r, n) }
```

⇒ **零新增依赖方向** ✓（`pkg/files` 只依赖自己拥有的接口 ✓；装配层依赖 `registry` ✓ —— 与
`files.VolumeSet`/`files.StorageManager` 同型 ✓）。

`registry.QuotaVolume` 侧（已有，只需补齐命名与语义）：

```go
Reserve(key, upperBound) (*Reservation, error) // 严格上界（两侧任一侧拒绝 ⇒ 全回滚）
Reservation.Commit(prev, written)              // 结算（覆盖写差分；两侧各自幂等）
Reservation.Release()                          // 双回滚
Footprint(owner, rel) (int64, bool)            // **卷自报**物理足迹（释放侧同源）
MaxPhysical(owner, rel, logical) int64         // 写前上界（**公式只写在卷里**）
```

**写内核（`files.Service`）的唯一流程**：

1. 路由（`routeUpload*`）→ 解析卷 + `Key` + 取 `Quota`；
2. **上界**：`bound := Quota.MaxPhysical(key.Owner, key.Rel, clientSize)`；
3. **预占**：`res, err := Quota.Reserve(key, bound)`（不足 ⇒ 507/排队重试）；
4. **写**：经 `Sink` 或 `Tenant.Root()`；
5. **结算**：`settled := Quota.Footprint(...)`（失败则 `bound`）→ `res.Commit(prev, settled)`；
6. **失败**：`res.Release()`。

### 13.3 为何这是**最佳形式**（对比当前形态）

| 维度 | 当前（`UploadRoute` 自带账本） ✗ | 目标（`QuotaVolume` 单一权威） ✓ |
|---|---|---|
| 记账职责 | 横切关注点**外泄到路由结构**（5 个字段 + 装配层 `rt.phys` 间接）✗ | 内聚在**卷**（唯一权威）✓ |
| 物理尺寸公式 | 散在调用方（`PhysSize` 由装配层注入）✗ | **只写在卷里**（`Footprint`/`MaxPhysical`）✓ |
| 双账本一致性 | 靠「调用点都记得同时 Commit 两侧」✗ | `Reservation` **一次覆盖两侧**（半入账不可表示）✓ |
| 整卷权威 | **按卷类型分治** ✗：本地卷 ⇒ `UploadRoute.Pool`；外部/封装卷 ⇒ `CapacityFS` （同一概念两个权威 ⇒ 改一处漏一处） | 统一到 `QuotaVolume`（`counter`/`pool` 单实例）⇒ **单本账** ✓ |
| 归属 | `(owner, rel)` 由调用方各自持有 ✗ | `LogicalKey` **显式下传**（封装卷亦可归属）✓ |
| 新增写面 | 必须复制 5 字段装配 ✗ | 只传 `Key` + 取 `Quota` ✓ |

### 13.4 迁移步骤（**原子**，一次成型）

1. **`registry.QuotaVolume` 补齐**：`Reserve` 返回 `*Reservation`（已有）；新增便捷
   `Settle(key, prev, written)` 与 `FootprintOr(key, fallback)`（**唯一**足迹口径）；
2. **`pkg/files.UploadRoute` 换形**：删 `Scope`/`ScopeRes`/`Pool`/`PoolRes`/`PhysSize`/`Release`，
   加 `Key`/`Quota`(=`QuotaAuthority` 窄接口)/`Res`(=`QuotaReservation`)；`Commit`/`Release` 改为
   委托 `Res`（**保留同名方法** ⇒ 现有 **52 处 `route.Release()` 调用点零改动** ✓）；
   新增领域侧窄接口 + `pkg/server` 适配器（依赖倒置，见 §13.2）；
3. **统一整卷权威**：把 `CapacityFS` 的池预占（7 处）与 `UploadRoute.Pool`（本地卷路径）
   **一并**收敛到 `QuotaVolume.Reserve` 的**整卷侧**（`VolumeCounter`/`pool` 单实例 ⇒ 单本账 ✓）
   —— **注意**：这不是「删重复」✗（前提已证伪 ✓），而是**消灭按卷类型分治的两个权威** ✓；
4. **装配层**（`pkg/server/files_service.go` 的 2 个适配 + `volumes.go` 的 4 个 `routeUpload*`）
   改为解析 `Quota` + `Key`；
5. **分块上传**（`chunked_store.go`/`chunked_upload.go`）：三套会话预留收敛为一套
   `*registry.Reservation`（`json:"-"` 句柄语义不变 ⇒ 重启后重算）；
6. **释放侧**（片 6）：`charged_bytes` 落 `FileMeta.Extra`（本地卷）/ 独立 ledger（外部卷），
   **删除** `prevCharge`/`chargedHint` 启发式 ⇒ 「释放 = 结算」成为**结构性不变式**。

### 13.5 验证策略（**必须有判别力**）

| 不变式 | 用例 | 变异（须红） |
|---|---|---|
| **恰好 1×（无 2×）** | 单次写入后 `scope.Usage()==size` 且 `pool.Usage()==size`，**本地卷与外部/封装卷结果一致** | 让两条权威路径**同时**记账 ⇒ 红（2×） |
| 覆盖写不双计 | `Commit(prev, written)` 后两侧 = `written` | 去掉差分 ⇒ 红 |
| 上界 ≥ 实测 | `MaxPhysical ≥ Footprint`（加密卷 + secretdata） | 用逻辑长度当上界 ⇒ 红 |
| 失败全回滚 | 写入失败后两侧**零残留** | 只回滚一侧 ⇒ 红 |
| 释放 = 结算 | 删除后两侧归零（**含 secretdata meta pad**） | 用逻辑长度释放 ⇒ 红（幽灵占用） |
| 零值键不记账 | `LogicalKey{}` ⇒ 两侧不变 + 可观测告警 | 去掉守卫 ⇒ 红 |

### 13.6 风险与回退

- **最大风险**：步骤 3（删 `CapacityFS` 池预占）与步骤 4（装配层）**同时**上线 ⇒ 若装配层漏改，
  整卷记账会**暂时消失**（少计 ⇒ 可超限）✗ ⇒ 必须由 §13.5 第一行的「无 `2×size` + 恰好 1×」
  双向断言钉住 ✓；
- **回退**：整体 revert（原子 ✓）；不留半成品形态 ✓；
- **不改**：`storage.Root` / 封装卷 / 卷类型装配（本片只动**记账归属** ✓）。
