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

### 🔴 P0-a：封装卷必须**经存储卷接口**读写（今天绕行 ✗）

实测：`enclosure.SourceFS`（`pkg/volume/enclosure/attach.go:166`）对**本地源卷**返回
**`syncpkg.NewLocalFS(rt.AbsPath(), nil)`** ✗ —— **裸路径 FS，绕过存储卷接口** ⇒
配额层**看不到**封装卷的写入 ✗✓。

⇒ 必须把 `SourceFS` 改为返回**存储卷的 FS 视图**（经 `RootFS`/卷接口 ✓，携带 `LogicalKey` ✓）。
**注意**：这与既有的 **R26「卷根直连适配器」棘轮** 同族 ✓（历史 P0 全出自「绝对路径 + 裸 os」✗）
⇒ 本片做完后，R26 白名单可**收缩** ✓。

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

## 8. 决议表（未定则不实施）

| # | 决议 | 建议 |
|---|---|---|
| 1 | 归属通道形态 | `LogicalKey` **参数**（非 context ✓，与既有「显式传递优于 context 隐式」一致 ✓） |
| 2 | 上界来源 | 卷自报 `MaxPhysical`（唯一可行 ✓） |
| 3 | 豁免清单 | staging / S3 part / meta sidecar / 索引快照 / probe / 云任务 —— 逐项**签字** ✓ |
| 4 | 配额层次序 | 在 `Wrap/Seal/Guard` **之下**（先拒后记 ✓）—— 逐卷确认 ✓ |
| 5 | 失败语义 | 半途失败**按实测结算** ✓（非释放 ✓） |
| 6 | 跨进程 | 要求 **per-volume 持久账本** ✓（否则 G1 不可宣称 ✗） |
| 7 | 迁移方式 | 严格「**先删上层、再加底层**」✓（并行期 = 双计 ✗，已实证 `committed += 2×size` ✗） |

---

## 9. 分片与验收矩阵

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
