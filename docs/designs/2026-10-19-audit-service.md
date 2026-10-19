<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一审计服务（`pkg/audit`）—— 作用域化 / 内存活跃索引 + 轮转 / 结构体能力 / WAL 续跑

> 状态：**设计 v2（经 3 路独立 fresh-context 对抗审核并逐条证伪后修订；待你确认后实施）**。
> 用户裁定（2026-10-19）：① 审计能力**必须由 `audit` 包提供**（禁到处实现）；
> ② 必须**抽象**，**所有审计都有各自的作用域**；③ **活跃记录在内存**加速访问，
> **内存超配置 / 内容过时 / 人工读取等场景立刻触发轮转**；④ 能力**必须由结构体提供**（禁全局变量）；
> ⑤ 生产可用后**必补各业务审计场景测试 + 自动恢复能力**，锁定防回归；
> ⑥ **包提供全部能力，使用方按场景选择性启用**（合规形态等为**可选能力**）；
> ⑦ 审计落盘**必须纳入配额**防磁盘打满；**后续支持归档到外部存储卷**；
> ⑧ 设计原则：**pkg 提供通用能力抽象，使用方灵活扩展**。

---

## 1. 现状与问题（证据，已逐条核实）

当前**两套**审计能力，互补但割裂：

| 维度 | A：`pkg/audit`（框架） | B：`pkg/server/audit*.go`（合规事件） |
|---|---|---|
| 语义 | 过程审计/诊断：步骤 span、资源、错误、跨步骤 kv | 合规审计：谁在何时对什么做了什么 + 结果 |
| 组织 | 按**作用域**：`<scopeID>.audit.log` 每 scope 一文件 | 按**全局时间线**：有界 ring + 单文件 + 轮转 |
| 持久化 | `FileSink`：JSON lines + **每行 fsync** + 0700 + 拒软链 + scopeID 白名单 | `AuditStore`：JSON lines + 轮转；**`_ = Append` 吞错**；**不 fsync**；**目录 0755 / 文件 0644** |
| 读面 | `Recent(Filter)`（包内） | `ring.Recent` + `/api/audit` + `/api/audit/export` + Web UI |
| 扩展 | `Sink` 可插拔、`Archiver` **预留未接线** | `notifyCenter.Dispatch`（通知中心） |
| 状态 | 跨步骤 kv（`Save/Get`）**仅内存** | 无作用域/状态概念 |
| 消费者 | cloud 任务、secretdata（encrypt/decrypt span）、transfer | **`RecordAudit` ≈115 处 / 20 文件**（非测试；另 files 域 `recordFileAudit` ≈29 处 / 3 文件） |

**结构性缺陷**：

- **P1-a**：合规事件**只有 `pkg/server` 能记** ⇒ 其他领域包要记必须依赖 server 或自造 ⇒ **不可能内聚复用**。
- **P1-b**：A 有 scope + fsync + 归档钩子但**无合规形态**；B 有合规形态但**无作用域、无重放、无终态**。
- **P1-c 覆盖缺口（实测，含审核补充）**——下列写面 `RecordAudit` = **0**：

  | 类别 | 具体 |
  |---|---|
  | 文件/目录 | `/upload`（**新文件成功**：仅 error 分支与 overwrite 分支有）· `/mkdir` `/rmdir`（`files/dirs.go`） |
  | 归档/回收站 | `/api/archive` · `/api/trash/restore` · `/api/trash/empty`（破坏性） |
  | 卷 | `/api/volumes/user` 建/删（`volumes_api.go` 的 11 处仅 move/rebalance） |
  | 密钥/签名 | `/api/secrets` 建删 · **`/api/backends/{type}/presign` + `/presign/complete`**（服务端凭据签任意键） |
  | **分享**（审核补充） | `POST /api/share`（`share.go` 只发 `EventShare`）· `DELETE /api/shares/{token}` |
  | **同步 HTTP**（审核补充） | `POST/DELETE /api/sync/tasks*` · `/api/sync/conflicts/{id}/resolve`（`sync_handler.go` = 0） |
  | **DAV 全部变更**（审核补充） | 不止 MKCOL/PROPPATCH —— `pkg/gateway/webdav` **零** `RecordAudit`（PUT/DELETE/MOVE/COPY/LOCK 同为零） |
  | 其它（审核补充） | cloud groups · `POST /api/notify/test`（`notify.go` = 0）· S3 multipart complete · `/api/tags` |
  | 同步引擎 | `pkg/syncexec` 全部写入（该包无任何审计调用点） |

- **P1-d 可靠性/可观测**：落盘失败与 ring 覆盖**无任何信号**（无 `audit_dropped` 计数、无 fail-closed 选项）；**无 request-id / 源 IP / volume / size / 耗时** 字段。
- **P1-e 无覆盖门禁** ⇒ 新增路由靠人记得加（上表漏项即根因）。
- **P1-f（审核 P0）读面无 ACL**：`/api/audit` 与 `/api/audit/export` 主 mux 仅 `authMiddleware`（`routes.go:515,520`），**无 `requireRole`**；`rbac_roles_test.go:455-458` 明确锁定「reader 可读属现状（200）」；localMux 侧**裸注册**（`routes.go:285,292`）。而 `AuditEvent` 含 `actor/mesh/object/detail`，设计再增 `source_ip/request_id/volume` ⇒ **跨租户/跨 mesh 的全局证据链可被最低权限主体读取**，且本设计把 secrets/presign/DAV/S3/sync 全量纳入审计后**泄露面显著放大**。⇒ 正式设计见 **§5.7 审计权限模型（两维）**。
- **P1-g（审核）保留 owner 名可被占用**：`validateRegistrationOwner`（`register_handler.go:1219`）只拒 `anonymous` 与保留桶名（user/meta/cloud/archive/chunk/version/trash）；`ValidSegmentName` 只拒 `.__` 前缀 ⇒ **`__audit__` 是合法可注册 owner** ⇒ §5.6 红线 1 形同虚设。
- **P2-a 轮转只按文件大小**（`audit.max_size`），无「内存超限 / 内容过时 / 读取触发 / 显式触发」。
- **P2-b `Save/Get` 只在内存** ⇒ 崩溃即丢 ⇒ **无法作为续跑锚点**。
- **P2-c（审核）落盘位置与权限与文档不符**：无 `audit.persist_dir` 配置键（仅注释），实际硬编码 `<默认卷根>/audit/audit.log`（`routes_setup.go:38-51`），目录 **0755**、文件 **0644**（世界可读）。
- **P2-d（审核）既有 TTL 会重写审计文件**：`volume_retention.go:199-213` 直接操作 `st.events/st.file/st.logPath` 并 `rewriteAuditLog` ⇒ 若与 WAL 段共用文件会**删除任务状态行**（破坏「任务不丢失」）。
- **P2-e（审核）既有测试与生产代码依赖 `AuditStore` 非导出字段** ⇒「实现下沉且用例原地通过」**不成立**（`volume_retention.go`、`audit_test.go`、`audit_rotation_test.go`）。

---

## 2. 目标与设计原则

1. **唯一提供方**：审计能力只由 `pkg/audit` 提供；其他包只**装配 + 使用**。
2. **一切审计皆有作用域**：`Open(scopeID)`；**`Emit` 定义在 `*Scope` 上**（`func (s *Scope) Emit(...)`）⇒ 用法上无法绕开 scope（**真编译期强制**；不再宣称 `Entry.Scope` 字段能做到）。
3. **活跃记录在内存 + 轮转**：内存超配置 / 内容过时 / **读取时** / 显式 / scope 结束 / 进程退出 ⇒ 立即轮转（§5）。
4. **结构体能力，零全局可变状态**：`*Service` 由装配层注入；`Options` 在 `New()` 时**一次性 resolve 成不可变 effective 配置**（消除每次 Emit 的三级查表与 nil 分支）；测试自建 service + MemoryJournal ⇒ 隔离。
5. **通用抽象 + 使用方扩展**：`Journal`/`Index`/`Notifier`/`Clock`/`Policy` 均为接口；每个可选接口配 **null-object**（`NullJournal`/`NullIndex`/`NullNotifier`）⇒ `Emit` 永不分支。
6. **包提供全部能力，使用方按场景选择性启用**（`CapabilitySet`）：合规形态、span、采样、WAL、归档、索引查询、通知、加密存储、配额治理**逐项可选**；测试与门禁按**少量规范剖面**（`compliance-only`/`telemetry-only`/`full`/`disabled`）而非全组合。
7. **分层（审核修订）**：`pkg/audit` **核心为叶子包**（只含 `Entry/Scope/Journal/Index/Notifier/Policy/Service`，**不 import 装配层、不 import `waitq`**）；**配额、归档、读面 ACL 均为外置适配器**（装配层或 `pkg/audit/adapters/*`）⇒ 不违反 R4 分层门禁。
8. **不阻断业务**（默认 best-effort）+ **可选 fail-closed**；任何失败**可观测**（计数 + 告警）。
9. **有界保留（审核修订）**：内存与磁盘**都**必须有上界；「不丢」限定为「**在配置上限内不丢**」，超限**显式丢弃 + 计数 + 告警**（不得无界增长）。
10. **幂等是使用方契约**（审核修订）：`Resume` 的 apply 函数**必须幂等**，由 `ReplayApplier` 接口契约 + 红灯用例锁定（库不提供幂等 ✗）。
11. **单机模型（审核修订）**：段在**各节点本地** ⇒ **跨节点恢复不在本期范围**；`Lease` 仅在「共享段存储」方案下才成立，本期**不引入**（避免自相矛盾）。
12. **读面 ACL（审核 P0）**：审计读面**必须**管理员角色（`audit.read`），并纳入 §5.6 红线。

---

## 3. 总体架构

```
                       ┌──────── pkg/audit（叶子核心：Entry/Scope/Journal/Index/Notifier/Policy/Service）────────┐
  使用方（任意包）      │  Service（结构体，注入式，无全局可变状态）                                         │
  ────────────────     │   ├─ Scope：Open/Close/MarkDone；**Emit 定义在 *Scope 上**（编译期强制作用域）      │
  scope.Emit(Entry{})  │   ├─ Span：Begin/End/Fail（过程审计）                                              │
  audit.From(ctx)      │   ├─ Save/Get：作用域 kv（**落盘** = WAL）· Resume/ListScopes（ReplayApplier 契约） │
                       │   └─ Rotate(reason)：显式轮转                                                      │
                       │      ├─ Journal（append-only；SegmentFileJournal / MemoryJournal / NullJournal）     │
                       │      ├─ Index（内存活跃索引，有界；RingIndex / NullIndex）                          │
                       │      ├─ Notifier（下游钩子；NullNotifier）                                          │
                       │      └─ Clock（Now + **Since**，可注入 ⇒ 过时/老化可测）                             │
                       └───────────────────────────────────────────────────────────────────────────────────┘
      ▲ 外置适配器（不在 pkg/audit 内，避免分层违规）
      ├─ 配额适配器：热段预留 / 冷段额度（装配层注册 `pkg/waitq.Resource`）
      ├─ 归档适配器：sealed 段 → 文件写内核 → 保留用户卷内路径（M4）
      └─ 读面 ACL：`/api/audit` `/api/audit/export` 必须管理员角色（M1-11）
装配层（pkg/server / cmd/sproxy）：构造 Service → 注入 Handlers/Manager/…；
                                挂 auditMiddleware（传输层兜底）+ 读面 ACL + /api/audit
```

| 层 | 职责 | 不做什么 |
|---|---|---|
| `pkg/audit` | 条目模型、作用域、内存索引、轮转策略、持久化抽象、重放、指标 | 不认识 HTTP、不 import 装配层/`waitq`、不做配额与归档 |
| 外置适配器 | 配额（waitq）、归档（写内核）、读面 ACL、通知渠道 | 不定义审计条目语义 |
| 使用方 | 发**语义**审计（action/object/volume/result/detail）+ 提供**幂等** ReplayApplier | 不碰落盘/轮转/索引 |

---

## 4. 数据结构与 API（细化）

### 4.1 条目模型（统一 `Row` 与 `AuditEvent`）

```go
type Kind string
const (
    KindEvent    Kind = "event"    // 合规事件（谁/什么/结果）
    KindSpan     Kind = "span"     // 过程步骤（含 dur/err）
    KindState    Kind = "state"    // 作用域状态（WAL：Save 落盘）
    KindDone     Kind = "done"     // 作用域终态（恢复时跳过）
    KindResource Kind = "resource" // 采样（CPU/内存/带宽）
    KindRotate   Kind = "rotate"   // 轮转标记（审计自身的可审计性）
)

type Entry struct {
    // 身份与顺序（**两级序**，见下）
    Scope ScopeID `json:"scope"`           // 必填
    Seq   uint64  `json:"seq"`             // **scope 内**单调（Resume 的权威顺序）
    Kind  Kind    `json:"kind"`
    // 兼容旧 Row 的维度（审核修订：补 Level/Dim ⇒ 旧文件可 round-trip）
    Level Level     `json:"level,omitempty"`
    Dim   Dimension `json:"dim,omitempty"`
    // 合规语义（**契约字段不加 omitempty** ⇒ /api/audit 字段出现性与现状一致）
    Action  string `json:"action"`
    Actor   string `json:"actor"`
    Owner   string `json:"owner"`            // **归属租户（权限过滤的权威字段；非 Actor）** —— 见 §5.7
    Mesh    string `json:"mesh"`
    ObjType string `json:"object_type"`
    Object  string `json:"object"`
    Result  Result `json:"result"`
    Detail  string `json:"detail"`
    Dim2    Dim2   `json:"dim2"`            // **两维**：user | system（§5.7）；现有 Dimension(DimTask/DimSystem) 保留兼容
    // 新增结构化字段（纯增量）
    Volume    string `json:"volume,omitempty"`
    RequestID string `json:"request_id,omitempty"`
    SourceIP  string `json:"source_ip,omitempty"`
    // 过程/资源
    Step string `json:"step,omitempty"`
    StartMS, DurMS int64 `json:"start_ms,omitempty"`
    Bytes, BwBps   int64
    Err  string `json:"err,omitempty"`
    Node string `json:"node,omitempty"`
    TS   time.Time `json:"ts"`
    Meta map[string]any `json:"meta,omitempty"`
}
```

**两级序（审核修订 P1-1）**：
- **scope 内**：`Seq` 是权威顺序（`Resume` 重放依赖它）；
- **全局段**（合规时间线）：per-scope `Seq` **不构成全序** ⇒ 全局序 = `(NodeID, 节点级单调 seq)`；`/api/audit` 倒序与 `/api/audit/export` 升序契约**保持不变**（按全局序，同值再按段内 offset 稳定）。
- `Filter`/`Query`/export 的排序语义在接口注释里**显式声明**，并有回归用例锁定。

**对外契约（必须保留，审核修订）**：`/api/audit` 的**信封 `{events,total}`** 与 export 的**顶层数组 + 升序**不变；Web UI 消费字段（`action/actor/mesh/object_type/object/result/detail/ts`）不变 ⇒ UI 零改动。

### 4.2 提供方接口（使用方扩展点）

```go
type Journal interface {
    Append(e Entry) error            // 成功 ⇒ 已按策略持久化
    Read(f Filter) ([]Entry, error)
    Segments() []SegmentInfo
    Rotate(reason string) error
    Rewrite(keep func(Entry) bool) error  // 审核修订：TTL/清理经接口（不再触碰非导出字段）
    Close() error
}

type Index interface {
    Add(e Entry)
    Query(f Filter) []Entry          // 排序语义见 §4.1（倒序 + limit 提前停）
    Len() int
    ApproxBytes() int64
    Evict(n int) []Entry             // 仅逐出**已持久化**条目
}

type Notifier interface{ Notify(ctx context.Context, e Entry) }  // 实现**不得 panic**（库侧仍 recover 兜底）

// Clock（审核修订：补 Since ⇒ 单调语义，供 waitq 老化与过时判定共用）
type Clock interface {
    Now() time.Time
    Since(time.Time) time.Duration
}

// ReplayApplier（审核修订 P1：幂等是**使用方契约**）
type ReplayApplier interface {
    // Apply 把一条 KindState 应用到业务状态。**必须幂等**：同一 (scope, Seq) 重复应用零副作用。
    Apply(ctx context.Context, scope ScopeID, seq uint64, state []byte) error
}
```

### 4.3 门面 API（结构体，注入式）

```go
type Options struct {
    Journal  Journal; Index Index; Notifier Notifier; Clock Clock
    Policy   Policy; Caps CapabilitySet; Layout Layout; NodeID string; Metrics Metrics
}
func New(opts Options) (*Service, error)   // 内部 resolve → 不可变 effective（null-object 兜底）

func (s *Service) Open(ctx context.Context, id ScopeID, meta ScopeMeta) (*Scope, error)
func (s *Scope) Emit(ctx context.Context, e Entry) error   // **scope 必填（接收者）** ⇒ 编译期强制
func (s *Service) Begin(ctx context.Context, step string, kv ...any) *Span
func (s *Service) Save(ctx context.Context, k string, v any) error  // WAL：按策略落盘
func (s *Service) Get(ctx context.Context, k string) (any, bool)
func (s *Service) Query(ctx context.Context, f Filter) ([]Entry, error)  // 读取 ⇒ 可触发轮转（§5.3）
func (s *Service) Resume(ctx context.Context, id ScopeID, ap ReplayApplier) (*ScopeState, error)
func (s *Service) ListScopes(ctx context.Context, f ScopeStateFilter) ([]ScopeInfo, error)
func (s *Service) MarkDone(ctx context.Context, id ScopeID) error
func (s *Service) Rotate(ctx context.Context, reason string) error
func (s *Service) Close() error
```

**兼容 facade（审核修订 P1-3：禁止包内双实现）**：旧 `NewScope/Logger/Row/Sink/FileSink/Span/Save/Get` 保留为**唯一实现之上的薄 facade**（`Row ↔ Entry` 单点转换；`NewScope` 内部构造 `Service` + 相应 Journal）⇒ 包内**一套实现、一个模型**；旧导出**只减不增**（门禁 R35-d），一个版本后删。

---

## 5. 轮转、落盘与存储策略

### 5.1 轮转触发（`Policy`；全部可注入 Clock 测试）

| # | 触发 | 配置键 | 说明 |
|---|---|---|---|
| T1 | **内存占用超限** | `audit.max_memory_bytes` / `audit.max_buffer_entries` | 超阈值 ⇒ flush + Rotate |
| T2 | **内容过时** | `audit.max_age` | 最早未落盘条目超龄 ⇒ Rotate（**惰性判定**，无后台 goroutine） |
| T3 | **读取触发（人工）** | `audit.no_rotate_on_read`（**默认 false = 默认轮转**，审核修订：bool 默认 false 纪律）+ `min_rotate_interval` | `Query`/`/api/audit`/export 发现「脏且超限/超时」⇒ 先轮转再返回 |
| T4 | **显式调用** | — | `Service.Rotate(reason)`（运维/信号/SIGHUP） |
| T5 | **作用域结束** | — | `Close()/MarkDone` ⇒ flush + seal |
| T6 | **进程退出** | — | `Service.Close()`（停服链） |
| T7 | **段大小超限** | `audit.max_size` / `audit.max_archives` | 既有行为（保留） |
| T8 | **热段达硬上限** | `audit.max_total_bytes` | **非零安全默认**（审核修订 P1-3）⇒ 立即轮转 + 触发归档（M4）或按 §5.4 有界丢弃 |

### 5.2 落盘策略：**包提供策略族，使用方按场景选择**

| 策略 | 语义 | 适用（使用方选择） |
|---|---|---|
| `SyncAlways` | 每行 fsync | 合规证据（**需显式开启**，见下）、WAL 终态 |
| `SyncBatch`（**默认**） | group commit：≤`batch_max_lines` 行或 ≤`batch_max_interval` 一次 fsync | 常规合规事件、span |
| `SyncNone` | 仅缓冲（轮转/退出时 flush） | 高频采样 |
| `Memory` | 只进内存索引 | 纯 UI 回看（显式放弃持久化） |

**审核修订 P1（性能）**：
- `KindEvent` **默认 `SyncBatch`**（现状本就**零 fsync**，`SyncAlways` 全量会把每次上传/删除/改名变成 fsync ⇒ 明确性能回归）⇒ `SyncAlways` 仅在合规部署**显式**开启；
- **段内单写者 goroutine + 有界追加队列**：`Service.mu` 只保护队列/段指针，**不跨 fsync**（避免把单行 fsync 放大成全站串行）；`SyncAlways` 经每条 ack 保持语义；
- 交付判据含**压测**（HDD/SSD 各一：吞吐与尾延迟）。

### 5.3 轮转语义（含审核修订的递归护栏）

- seal 当前段 → 开新段；归档保留 `max_archives` 份；
- 轮转写一条 `KindRotate`（reason ∈ `memory_limit|age|read|manual|scope_end|shutdown|size|disk_full`）；
- **段切换耐久窗口（审核 E24）**：**先 open + fsync 新段，再 seal 旧段**（或缓冲行分别 fsync 到归属段）⇒ 已 `Append` 成功的行在崩溃后必须可见；
- **递归护栏（审核 E22）**：`Rotate` 内部写 `KindRotate` 时**跳过节流与再次轮转判定**（进入旗标）⇒ `Query→Rotate→Emit→Rotate…` 不可能；
- **防抖**：`min_rotate_interval` 内不重复触发。

### 5.4 存储归属与配额治理（**审核修订：诚实承诺 + 有界保留**）

| 层 | 位置 | 配额机制 | 说明 |
|---|---|---|---|
| **热段（本地）** | 默认 `<默认卷根>/audit/`（**M1 收紧为目录 0700 / 文件 0600**） | `audit.local_reserve_bytes`（**保证可写额度**）+ `audit.max_total_bytes`（**硬上限，非零安全默认**） | 达上限 ⇒ T8 轮转 + 归档；归档不可用 ⇒ **有界丢弃**（§5.4.1） |
| **冷段（卷内归档，M4）** | 保留用户卷内路径 | 走**卷 / 用户配额**（与用户数据同机制） | 超限按策略（丢最旧 / 停归档 + 告警） |

**5.4.1 有界保留（解决 E3×E4×T1 矛盾，审核 P1-5）**

不变式：**内存与磁盘都有上界；「不丢」= 「在配置上限内不丢」**。

1. 内存达 T1 上限且**无法持久化**（磁盘满/Journal 持续失败）⇒ **丢弃最旧** + `audit_dropped++` + 告警 + `KindRotate(reason=disk_full)`（**不得无界增长 ⇒ OOM**）；
2. `Evict` 优先逐出**已持久化**条目；仅当「未持久化条目已达内存上限」才丢未持久化条目（同上计数）；
3. `fail_closed=true` ⇒ 该变更操作**快速失败**（不静默丢）。

**5.4.2 关于「预留」的诚实表述（审核 P1-5）**：`local_reserve_bytes` **只保证「审计自身不因膨胀而失控」**，**不保证**「用户把共享文件系统写满后审计仍可写」（无 `fallocate`/独立分区时不可能 ✗）。⇒ 需真保证时：**热段置于独立分区/卷**，或用 `fallocate` 预分配；本设计**默认按「自身上界」语义**并在文档/配置注释里写清（不再过度承诺）。

### 5.5 归档到外部存储卷（M4；**审核修订：外置适配器**）

1. sealed 段（+ 摘要：起止 TS/Seq/条目数/校验和）→ **归档适配器**（装配层）经**文件写内核**写入保留用户路径 ⇒ 复用**配额/加密/版本/回收站/多卷**；
2. 归档成功 ⇒ 热段按 `audit.local_keep_segments` 回收；失败 ⇒ **保留热段 + 重试 + 告警**（绝不静默丢）；校验和不符 ⇒ 视为失败（E19）；
3. 归档动作写 `action=audit.archive`（**审计的审计**）；
4. **归档↔轮转并发护栏（审核 E23）**：归档对目标段持**引用/占用判据**；轮转回收热段前校验「未被归档占用」；
5. `Query` 跨热/冷段（按摘要定位，不扫全量）；**加密卷只解决静态加密，不解决跨用户可读性**（可读性由读面 ACL 决定 —— 审核 P2）。

### 5.6 审计数据的保留用户（系统用户）—— 推荐方案（**含审核修订的前提条件**）

**推荐：是**（保留 owner + 混合存储：热段本地 append-only + 冷段归档到保留用户卷内路径），理由：复用配额/加密/外部卷/版本/回收站/统一恢复。

**前提条件（审核 P1，必须先做，否则红线形同虚设）**：
1. **保留该名字**：`validateRegistrationOwner`（`register_handler.go`）与 AK/owner 入口**保留 `__` 前缀（至少 `__audit__`）**，fail-closed 拒绝注册；补单测；
2. **段名层**：`ValidSegmentName`/`IsReservedBucketName` 增 `__` 保留前缀判定（或由 ① 承担）；
3. 命名建议 `__audit__`（待裁定 O6）。

**安全红线（实现时锁死 + 测试矩阵）**：
1. 普通用户**不可读/写/删**该 owner 路径（文件面 / DAV / S3 / 分享 / 回收站 / 版本 / 卷导出导入 / 备份 全部拒绝 403）；
2. **不在 Web UI 文件列表**展示（或仅管理员「审计存储」页）；
3. **不可通过分享 / 直链 / presign 外发**；
4. 该 owner 的**配额**单独设定（不受用户配额回收影响）；
5. 该 owner 的写操作**不得再产生审计条目**（`Meta["no_audit"]=true`，E16）；
6. **（审核 P0）审计读面权限**：**两维权限模型**（用户维度 / 系统维度）—— 正式设计见 **§5.7**（非管理员**只能**读自己的维度且**脱敏**；管理员全量；过滤**服务端强制**、不可由客户端放宽）；
7. **（审核）TTL 只作用于合规全局段**，**绝不触碰** WAL/任务段（E25）；
8. **（审核）权限收紧**：热段目录 0700 / 文件 0600（含既有文件与 `AuditStore` 的 0755/0644 收口）；
9. **（多管理员）审计读**也要审计**：`action=audit.read`（系统维度）⇒ 管理员之间的读取可追溯；
10. **（多管理员）归档卷是系统级配置**（`audit.archive_volume`），**不绑定任何管理员**；
11. **逃生舱（默认关闭）**：`audit.archive_owner` 默认 `__audit__`；若部署坚持存到某 owner，可显式设为其值，但**必须接受该 owner 的配额/生命周期/删除语义**（文档标注风险）。

### 5.7 审计权限模型（两维：用户维度 / 系统维度）—— **审核 P0 的正式设计**

**问题**：现状 `/api/audit` 与 `/api/audit/export` 仅认证、无角色门 ⇒ 最低权限主体可读**跨租户全局证据链**（`rbac_roles_test.go:455-458` 还把它锁成契约）。

**设计（两维 + 服务端强制 + 脱敏）**：

| 维度 | 内容 | 可见性 |
|---|---|---|
| **用户维度**（`Dim2=user`） | 该 owner 自己的数据面操作：upload/overwrite/rename/copy/move/delete/软删/mkdir/rmdir/archive/trash/version/tags/分享创建/自己的任务 | **owner 本人**可读（**脱敏**）+ 管理员全量 |
| **系统维度**（`Dim2=system`） | 跨租户/运维/安全：config、credentials、volumes（含 user volume 建删）、quota set、presign、retention、tier、node/mesh 生命周期、审计自身（rotate/archive） | **仅管理员** |
| **跨维条目**（`Dim2=system` 且 `Owner=<目标>`） | 管理员对该 owner 的处置（配额调整、卷变更、强制删除…） | **owner 本人可见但脱敏**（action/result/ts）⇒ 透明性；管理员全量 |

**判定权威字段 = `Entry.Owner`**（**不是** `Actor`）：`Actor` 是访问键（AK/SproxySig/mesh），`Owner` 才是数据归属；二者可能不同（管理员/中继代表他人操作）⇒ **过滤必须用 `Owner`**。

**服务端强制（不可由客户端放宽）**：
1. 非管理员 ⇒ 处理器**强制注入** `Filter.Owner = <caller owner>` 且 `Dim2 ∈ {user, system-with-my-owner}`；**忽略**客户端传入的 `owner`/`dim`（或取**交集**，永不并集）；
2. 管理员 ⇒ 可省略 owner（全量）或指定任意 owner；
3. **脱敏规则**（非管理员）：仅返回 `action/object_type/object/result/ts/detail(脱敏)`；**不返回** `actor/mesh/source_ip/request_id/node/meta`；
4. `export` 与 `list` **同一门 + 同一过滤**（不得只保护 list）；
5. localMux 侧纳入同一门（现状裸注册）；
6. 未认证 ⇒ 401（不变）；已认证但越权 ⇒ 403。

**必要性分析（用户提问）**：
- **对核心目标（断点恢复 / 功能可靠）**：**不必要** —— `Resume/ListScopes` 与用户读面无关，审计读面**不参与**恢复路径；
- **对易用性**：有价值（用户自查「我的文件为何被拒 / 任务到哪了」、降低支持成本、与「操作历史」UX 一致）；
- **对安全**：用户维度视图**必须**依赖「权威 `Owner` 字段 + 服务端强制过滤 + 脱敏」三项，缺一即不可开放（否则等于把跨租户审计暴露给用户）；
- **结论：两维模型现在设计、分两步实施**：
  - **M1-11（P0，立即）**：管理员门（`requireRole(admin)`）+ 服务端强制 owner 过滤骨架 + 脱敏层接口；此时非管理员**只能读自己的维度**，若该视图未就绪则返回**空集**（**安全默认**：绝不在未就绪时回退全量）；
  - **S4/后续（低优先级）**：用户维度视图（Web UI 页 + 筛选 + 脱敏渲染 + 真浏览器 e2e）。

### 5.8 存储归属决策：**保留系统 owner** vs 管理员 owner（多管理员扩展性分析）

**你问的**：是否推荐专门的审计用户？直接存到管理员存储是否后续难以扩展多管理员？

**结论**：**推荐保留系统 owner（`__audit__`）**；**存到管理员 owner 会显著妨碍多管理员扩展** ✗。

**现状事实（取证）**：
- 角色来自 AK（`admin/user/reader/node`，`requireRole` 分组判定）⇒ **多管理员天然支持**（N 个 AK 带 `Role=admin`）；
- **`owner = ak`**（`s3_server.go:132,340`、`register_handler.go:389`）⇒ **每个管理员是不同 owner**；
- **配额按 owner 记账**（`Pool.EnsureScope(segs…)`）；
- 系统侧目前**只有 `anonymous` 一个保留 owner**，无系统 owner 概念。

**若存到管理员 owner ⇒ 四个真实障碍**：

| # | 障碍 | 本质 |
|---|---|---|
| 1 | **归属歧义** | 多个管理员时“存到哪个管理员”无客观答案；换人/换班就要迁数据 |
| 2 | **可见性变成“所有权”而非“角色”** | 管理员 B 要看管理员 A 名下的审计 ⇒ 需**跨 owner 访问**，而全仓数据面不变式是「认证主体 = owner」⇒ 必须**逐面特例化**（破坏统一模型） |
| 3 | **凭据生命周期污染证据链** | AK 可轮换/注销/删除 ⇒ 审计数据归属跟着断（孤儿/丢失）；**审计不能绑在临时身份上** |
| 4 | **配额与删除语义** | 审计增长计入该管理员**个人配额**（可阻塞其自己的文件）；用户面的删除/回收站/版本/保留策略都会作用到审计数据 ⇒ **证据可被删** |

**保留系统 owner 如何解决（且对多管理员友好）**：

| 维度 | 保留系统 owner（推荐） | 管理员 owner |
|---|---|---|
| 多管理员可见性 | **角色/能力位**决定（RBAC）⇒ 加管理员 = 改凭据，**零存储变更** | 所有权决定 ⇒ 加管理员需授权/迁移 |
| 管理员轮换/注销 | **零影响**（与身份解耦） | 归属断裂风险 |
| 配额 | **独立配额节点**（不占任何人的个人配额） | 占该管理员配额 |
| 删除语义 | 无文件面、无用户面删除路径 ⇒ **不可篡改/不可删** | 按该 owner 的用户面规则（可删） |
| 加密/外部卷/版本/回收站 | **复用**（归档走写内核） | 复用 |
| 身份 | **无凭据、不可登录、不可注册** | 需绑定一个真实管理员身份 |

**关键区分**：**保留 owner（存储键空间）≠ 用户账号（身份）** —— 审计数据**不需要身份**，只需要一个**稳定的命名空间**；给审计开一个能登录的账号只会增加**权限提升面**与凭据管理负担 ⇒ **不推荐专门审计账号**，推荐**保留 owner**。

**多管理员下的额外设计点**：
1. **权限位优于单角色**（已修订 Q12）：M1-11 门禁按 **`admin` 或新增能力位 `audit.read`** —— 未来可加「审计员（只读、无管理权）」角色而**不必授予 admin** ✓（这正是多管理员/多角色扩展性的关键）；
2. **审计读也要审计**（红线 9）：`action=audit.read` 入系统维度 ⇒ 多管理员之间的读取可追溯；
3. **归档卷不绑管理员**（红线 10）；
4. **`owner` 判定用 `Entry.Owner`**（§5.7）—— 与“管理员/中继代表他人操作”一致。

**逃生舱**（可选，默认关闭）：`audit.archive_owner`（默认 `__audit__`）—— 若部署坚持存到某 owner，可显式指定，但需接受该 owner 的配额/生命周期/删除语义（红线 11）✓。

---

## 6. WAL 与「中断继续」

**同一份日志承载两类事实**：合规事件 + 任务状态 ⇒ 恢复重放同一日志（且恢复动作本身是审计条目）。

### 6.1 状态机与**提交语义（审核 P1：MarkDone 顺序）**

```
Open(scopeID) ─▶ active ─(Save 落盘 KindState, Seq++)─▶ active
                   ├─(崩溃)─▶ 重启扫描 ⇒ unfinished（末行非 KindDone）⇒ Resume(ReplayApplier)
                   └─(MarkDone)─▶ done（KindDone 落盘 ⇒ 恢复跳过）
```

**契约**：`MarkDone` = **副作用完成之后的提交记录（commit record）**：
- **副作用先做、`MarkDone` 后写** ⇒ 之间崩溃 ⇒ 重放（**依赖使用方 apply 幂等** ✓ 由 `ReplayApplier` 契约保证）；
- **禁止**先 `MarkDone` 后做副作用（那会「跳过未完成的任务」⇒ 真丢）；
- 该顺序写入接口注释 + 红灯用例（`MarkDone` 前 kill ⇒ 重放完成；`MarkDone` 后 kill ⇒ 跳过）。

### 6.2 恢复原语

| 原语 | 语义 |
|---|---|
| `ListScopes(unfinished)` | 扫段目录 ⇒ 未完成作用域（含最后 Seq/TS/摘要） |
| `Resume(scopeID, ReplayApplier)` | 按 **Seq** 重放 `KindState`；**apply 必须幂等**（同 Seq 重复应用零副作用） |
| `MarkDone(scopeID)` | 写终态（提交记录，见 §6.1） |
| `Lease` | **本期不引入**（段在本地 ⇒ 跨节点恢复不在范围；共享段存储方案另议 —— 审核 P1） |

### 6.3 与 M3 统一恢复协调器的关系

`docs/archive/architecture-task-recovery.md`（用户 2026-10-10 裁定）要求统一恢复协调器收敛 5 处独立实现。本设计的 `ListScopes/Resume` 是**注册与重放原语**；M3 = 在其上建**协调器 + 注册中心**。
**兼容兜底（审核 A5）**：M3 首版**保留 legacy 扫描兜底**（无 WAL 状态的在途任务回落现有 `recover*`），一个版本后删。

---

## 7. 边界场景（E1–E26）

| # | 场景 | 处理 |
|---|---|---|
| E1 | 无作用域的审计 | `Emit` 在 `*Scope` 上 ⇒ 请求路径天然有 scope；**后台/周期作业必须自开合成 scope**（`system:<job>`，如 `volumes_mirror`、`tier_manager`、`verifyPeriodicTask`、`cmd/sproxy` 启动期）——**不是拒绝而是强制开 scope**（审核 P1：否则薄委托会丢审计） |
| E2 | 轮转与并发 Emit | 段切换持 `Service.mu`（仅指针/队列，**不跨 fsync**）；切换期间行入新段队列 |
| E3 | fsync 失败 | 计数 + 告警 + 重试；达内存上限 ⇒ **有界丢弃**（§5.4.1）；`fail_closed` ⇒ 快速失败 |
| E4 | 内存逐出 vs 未落盘 | 优先逐出已持久化；未持久化达上限才丢（计数）⇒ **内存有界**（§5.4.1） |
| E5 | 崩溃后半行/坏行 | 丢弃不完整尾行与解析失败行 + `audit_corrupt_lines` 计数 |
| E6 | T2 过时轮转实现 | 惰性判定（无 goroutine；可注入 Clock） |
| E7 | 时钟回拨 / 同 TS | scope 内顺序 = `Seq`；全局序 = `(NodeID, 节点 seq)`（§4.1） |
| E8 | scopeID 路径安全 | 白名单 + 0700/0600 + 拒软链（沿用 `ensurePrivateDir`） |
| E9 | 多节点 | **单机模型**：段本地、跨节点恢复不在本期；不做无 fencing 的 `Lease` |
| E10 | 敏感内容 | 禁止写密钥/凭据 ⇒ **测试锁定**（敏感字段名扫描断言） |
| E11 | 读取即轮转抖动 | `min_rotate_interval` 冷却 |
| E12 | 审计阻塞业务 | 默认 best-effort + **单写者 + 有界队列**；`SyncBatch` 默认；压测判据 |
| E13 | 禁用态 | `disabled` 剖面 ⇒ no-op Service（`Emit` 静默成功 + 计数），零回归 |
| E14 | 迁移期双格式 | 旧 `AuditEvent`/`Row` 文件与新 `Entry` **可并存读取**（`Level/Dim` 已补 ⇒ round-trip） |
| E15 | 总容量上限 | `max_total_bytes` **非零安全默认** + T8 |
| E16 | 审计写审计（递归） | `Meta["no_audit"]=true` ⇒ 写内核跳过审计 |
| E17 | 配额不足 / 自举兜底 | 热段达上限 ⇒ 轮转 + 归档；归档失败 ⇒ **有界内存 + 计数丢弃**（§5.4.1） |
| E18 | 保留 owner 被触碰 | 一律 403 + `action=audit.retained_owner_violation` |
| E19 | 归档部分成功 / 校验和不符 | 视为失败：保留热段 + 重试 + 告警（**不得删热段**） |
| E20 | 热段被外部删除/截断 | 段连续性校验（Seq 缺口/摘要）⇒ `audit_corrupt_segments` 计数 + 告警，不阻断启动 |
| **E21** | **审计路径自身 panic**（Notifier/Index 使用方实现） | 库侧 **recover 包裹** + `audit_hook_panics` 计数 ⇒ 绝不把业务打崩 |
| **E22** | **Query→Rotate→Emit 递归** | 轮转进入旗标 + `KindRotate` 写入跳过节流/递归判定 |
| **E23** | **归档↔轮转并发** | 目标段引用/占用判据；回收前校验未被归档占用 |
| **E24** | **seal→open 崩溃窗口** | 先 open+fsync 新段再 seal 旧段 ⇒ 已 Append 行崩溃后可见 |
| **E25** | **TTL 重写 vs WAL 段** | TTL/清理只作用于合规全局段（经 `Journal.Rewrite` 接口），**绝不触碰** WAL/任务段 |
| **E26** | **读面无 ACL（审核 P0）** | **两维权限模型**（§5.7）：非管理员只能读自己的维度 + 脱敏；管理员全量；`reader ⇒ 403`（系统维度）/ 空集（用户视图未就绪）用例 |

---

## 8. 迁移与门禁

### 8.1 迁移步骤（审核修订：依赖与前提显式化）

| 步骤 | 内容 | 验证 |
|---|---|---|
| M1-1 | `pkg/audit` 核心（`Entry/Kind/Service/Scope/Journal/Index/Notifier/Clock/Policy/CapabilitySet`）+ null-object + `Options.resolve` | 新包单测；旧测试全绿 |
| M1-2 | `pkg/server.RecordAudit` **薄委托**（字段映射等价、scope 语义新增）；**后台/周期审计点补合成 scope**（E1） | 现有 863+519+… 行用例通过（见 M1-3 限制） |
| M1-3 | **`AuditStore`/`AuditRing` 迁移（已裁定：方案②彻底）**：类型**迁入 `pkg/audit`**（`audit.EventStore` / `audit.RingIndex`）；`volume_retention.go` 的内部字段访问改为 **`Journal.Prune(cutoff)` / `Index.Evict`** 接口；白盒测试（`audit_test.go:321-323`、`audit_rotation_test.go` 的 `logPath/archivePath`、`audit_handler_test.go` 的 `auditRing.Recent`）随之迁移或改经公开访问器（`Segments()/ArchivePath(n)`）；**删除「原地通过」表述** | 迁移完成 + `volume_retention.go` 不再触碰非导出字段 + 现有语义（倒序/limit/export 升序/轮转文件名与份数/信封）逐条保留用例通过 |
| M1-4 | **覆盖缺口补齐**（§1 P1-c **全表**，含 share/sync-HTTP/DAV 全部变更/cloud groups/notify/test）+ `auditMiddleware` 传输层兜底 | 新增业务审计用例；**兜底事件标 `fidelity=transport` 且不计入覆盖达标**（审核 P1-2） |
| M1-5 | 结构化字段（volume/request_id/source_ip/size/dur）+ 丢弃/失败/panic 计数 + `fail_closed` 选项 | 用例 + 指标断言 |
| M1-6 | 轮转四触发 + 读取即轮转 + 递归护栏（E22）+ 段切换耐久（E24） | 可注入 Clock 的确定性用例 |
| M1-7 | 删旧散点（棘轮收敛）+ **旧 API 降级为 facade**（R35-d） | 门禁 R35 通过 |
| M1-8 | **（依赖 W1）** 热段配额治理（`audit.local` 经外置适配器注册 `waitq.Resource`）+ 有界保留（§5.4.1） | 配额/有界保留用例 |
| M1-9 | **保留 owner 前提**（§5.6 前提 1–3）+ 拒绝面（红线 1–5） | 越权矩阵 403 用例 |
| M1-10 | `CapabilitySet` + 策略族选择 + 规范剖面 | 各剖面生效断言 |
| **M1-11** | **读面权限（审核 P0）**：管理员门（`requireRole(admin)`）+ **服务端强制 owner 过滤骨架 + 脱敏层**（§5.7）；主 mux + localMux 同一门 | `reader`：系统维度 ⇒ 403；用户维度未就绪 ⇒ **空集**（绝不回退全量）；`owner` 参数不可放宽过滤 |
| **M1-12** | **权限收紧**：热段 0700/0600（含 `AuditStore` 0755/0644 收口 + 既有文件收紧） | 权限位断言 |
| **M1-13** | **TTL 与 WAL 隔离**（E25）：清理只作用于合规全局段 | 用例：TTL 重写后 WAL 段完好 |
| **M4（后续）** | 归档到外部/加密卷（外置适配器）+ 跨热/冷段 `Query` | 归档往返（含加密/外部卷）+ 失败不丢热段 |

### 8.2 门禁（结构门禁 > 运行时用例）

| 门禁 | 断言 |
|---|---|
| R35-a | `Emit`/`RecordAudit` 调用点必须在登记表（**基线按实测：`RecordAudit` 115 处/20 文件 + `recordFileAudit` 29 处/3 文件**）；棘轮只减不增 |
| R35-b | **所有变更型路由**必须经 `auditMiddleware`（或登记豁免 + 理由）；**transport 兜底事件不计入覆盖达标** |
| R35-c | `pkg/server` 不得定义审计条目类型/落盘实现（只装配） |
| **R35-d** | `pkg/audit` **不得同时导出两套 span/kv API**（除 facade） |
| **R35-e** | `/api/audit` `/api/audit/export` 必须：① 管理员门；② **服务端强制 owner 过滤**（非管理员不得通过参数放宽）；③ `export` 与 `list` 同一门与同一过滤（或登记豁免 + 理由） |
| R37（M2 后） | 长任务必须 `Open` 作用域（可续跑） |
| **门禁登记表** | 新增 `docs/gates.md`（R 号 / 文件 / 断言 / 基线），并加测试断言「每个 R 号在代码注释与登记表双向出现」（审核 C6） |

---

## 9. 测试计划（用户要求⑤：生产可用后必补 + 锁定防回归）

### 9.1 各业务功能「审计场景」用例（逐项断言 action/actor/object/volume/result）

文件（upload **新增** / overwrite / rename / copy / move / delete / 软删，含 denied/error）· 目录（mkdir/rmdir 含配额不足 denied）· 归档 archive（含 target volume）· 回收站（restore/empty 单卷与 `all=1`）· 卷（user volume 建删、move/rebalance/copy/import、quota set）· 密钥（**断言不含密钥明文**）· **presign(+complete，含被拒)** · **分享创建/撤销** · **sync HTTP（任务/冲突）** · **DAV（PUT/DELETE/MOVE/COPY/MKCOL/PROPPATCH/LOCK）** · 版本（restore/delete）· 云任务（cancel/delete/archive、cloud groups）· **syncexec 拉/推** · 传输层（任意变更路由 ⇒ ≥1 条事件含 request_id/source_ip）· **保留 owner 越权矩阵（读/写/删/分享/回收站 ⇒ 403）** · **读面权限（§5.7）**：非管理员 ⇒ 系统维度 403 / 用户维度仅见自己且**脱敏**（断言不含 actor/source_ip/request_id）/ 客户端传 `owner` 不能放宽 · **owner 判定用 `Owner` 而非 `Actor`**（管理员代表他人操作时可见性正确）。

### 9.2 恢复与可靠性用例（审核补充）

- **崩溃重放（真实 kill，非同进程）**：`test/e2e` 起真实二进制 + 子进程 kill ⇒ 重启 ⇒ `ListScopes(unfinished)` 命中 ⇒ `Resume` 重建 ✓（含**撕裂行注入**：人为截断段文件尾行）；
- **幂等契约红灯**：注入**非幂等** `ReplayApplier` ⇒ 二次 Resume 断言「副作用只发生一次」失败（锁定契约）；
- **提交语义**：`MarkDone` 前 kill ⇒ 重放完成；`MarkDone` 后 kill ⇒ 跳过；
- **轮转不丢**：并发 Emit + 同时 T1/T3 ⇒ 条目总数 = Emit 数；
- **内存有界 + 丢弃计数**：注入永久失败 Journal ⇒ 内存不超上限 + `audit_dropped` == 超出数 + 业务不受影响；
- **不阻断业务的量化判据**：注入 500ms 慢 Journal ⇒ 业务 handler 延迟不受影响（P99 阈值断言）；
- **panic 隔离**：注入 panicking Notifier/Index ⇒ 业务成功 + `audit_hook_panics`+1；
- **禁用态隔离**：`disabled` 剖面 ⇒ `Emit` no-op 且**不影响其它测试**；
- **隔离性（要求④）**：两个测试各自 `New()` ⇒ 互不影响；门禁扫描 `pkg/audit` **可变**包级状态（白名单：`errors.New` 哨兵、`regexp.MustCompile`、不可变常量表）⇒ **不误报** `var ErrNoSink`/`var scopeIDPattern`（审核 P2）。

### 9.3 测试纪律

`t.Parallel()` 默认 · **禁新增 `time.Sleep`**（R14 棘轮；用注入 Clock / `Stats()`）· serial 预算棘轮 · 每包必须有测试。

---

## 10. 能力矩阵（审计）

| 能力 | 现状 | 目标 | 归属 |
|---|---|---|---|
| 合规事件 | server 私有 | `(*Scope).Emit` | 全包可用 |
| 作用域（编译期强制） | 部分 | `Emit` 定义在 `*Scope` | ✓ |
| 内存活跃索引（快速访问） | 有（无 scope） | `Index`（倒序 + limit + 过滤语义锁定） | ✓ |
| 轮转 T1/T2/T3/T4/T5/T6/T7/T8 | 仅大小 | 全 8 触发 | ✓ |
| 落盘策略族 | 硬编码/尽力 | 4 策略 + **默认 SyncBatch** | ✓ |
| 归档到外部/加密卷 | 预留 | M4 外置适配器 | ✓ |
| WAL（Save/Seq/Resume/ListScopes/MarkDone） | 无 | §6（幂等契约 + 提交语义） | ✓ |
| 传输层兜底（覆盖完备） | 无 | `auditMiddleware`（不计达标） | 装配层 |
| 结构化字段 | 部分 | volume/reqID/IP/size/dur | ✓ |
| 有界保留 + 丢弃/失败/panic 计数 | 无 | §5.4.1 + `Metrics` | ✓ |
| **读面 ACL** | **无（P0）** | `requireRole(admin)` | 装配层 |
| **保留 owner 名保护** | **可被占用** | 注册入口保留 `__` | ✓ |
| 权限收紧 | 0755/0644 | 0700/0600 | ✓ |
| 无全局可变状态 / 可注入 Clock | 混用 | 全结构体（门禁 + 白名单） | ✓ |
| 能力选择性启用（CapabilitySet） | 无 | 规范剖面 | ✓ |
| 分层（核心叶子 + 外置适配器） | — | 不 import 装配层/waitq | ✓ |

---

## 11. 开放问题

| # | 项 | 建议 |
|---|---|---|
| O6 | 保留 owner 命名 | `__audit__`（前提：注册入口保留 `__` 前缀） |
| O7 | `local_reserve_bytes` 默认 | 256 MiB（**语义 = 自身上界，非物理预留**） |
| O8 | 归档触发阈值 | 热段 > 预留 70% 或 sealed 段 > N |
| O9 | 本地热段是否加密 | 先明文 + 0700/0600；M4 归档统一走加密卷 |
| O10 | `max_total_bytes` 默认 | **非零安全默认**（如 `local_reserve_bytes × 4`）；部署可覆盖 |
| O11 | 单机模型是否接受 | 建议接受（跨节点恢复另立专题，含共享段存储 + fencing） |
| ~~O12~~ | ~~读面角色~~ | **已裁定（2026-10-19）**：采用 **§5.7 两维权限模型**（用户维度可读自己且脱敏 / 系统维度仅管理员 / 服务端强制过滤）；M1-11 先落管理员门 + 过滤骨架（安全默认），用户维度视图低优先级后续 |

---

## 12. 修订记录（v2，经 3 路独立审核并逐条证伪）

| 审核项 | 处置 |
|---|---|
| revC P0：审计读面无 ACL（`reader` 可读全量） | **新增 M1-11 + 红线 6 + E26 + R35-e + 用例**（`reader ⇒ 403`） |
| revC P1：`__audit__` 可被注册占用 | **新增 §5.6 前提 1–3 + M1-9**（注册入口保留 `__`） |
| revC P1：`max_total_bytes` 默认 0=不限（自相矛盾） | **改非零安全默认**（T8/O10） |
| revC P1：`SyncAlways` 默认 ⇒ 性能回归 + 全局锁串行 | **默认改 `SyncBatch`** + 单写者 goroutine（§5.2） |
| revC P1：`pkg/audit` 分层矛盾（叶子 vs waitq/写内核） | **核心叶子 + 外置适配器**（§2.7/§3/§5.5） |
| revA/revB P1：E1 与后台审计点冲突 | E1 改为**强制开合成 scope**；M1-2 补后台点 |
| revB P1：WAL 幂等被写成库保证 | **`ReplayApplier` 契约 + 红灯用例**（§4.2/§6.2） |
| revB P1：`MarkDone` 顺序未定义 | **定义为副作用后的提交记录**（§6.1） |
| revB P1：「ring/store 用例原地通过」不成立 | **M1-3 改为二选一方案** + 删该表述 |
| revB P1：waitq I1 对 quota 适配不可达 | **waitq.md 修订**：释放侧显式接线 |
| revB P1：热段「预留」过度承诺 | **§5.4.2 诚实表述**（自身上界 ≠ 物理预留） |
| revB P1：多节点本地段 + Lease 矛盾 | **单机模型**（E9/O11） |
| revA P1-1：per-scope `Seq` vs 全局段无全序 | **两级序**（§4.1/E7） |
| revA P1-2：覆盖清单不完整 + 兜底假绿 | 补 share/sync-HTTP/DAV 全部/cloud groups/notify；**transport 事件不计达标** |
| revA P1-3：包内两套 API 并存 | **旧 API 降级 facade + R35-d** |
| revA P1-4：0700 与现状不符 + `persist_dir` 不存在 | 现状改述 + **M1-12 权限收紧** |
| revA P1-5 / revB P2：E3×E4×T1 矛盾 | **§5.4.1 有界保留**（显式丢弃 + 计数） |
| revA P1-6：fsync 全站串行 | §5.2 单写者 + 压测判据 |
| revA P1-7：阶段依赖不自洽（M1-8 需 W1） | M1-8 标**依赖 W1**；M1-9 收窄 |
| revA P2-4 / revB P2：TTL 重写会删 WAL 行 | **E25 + M1-13**（`Journal.Rewrite` 只作用合规段） |
| revA P2-5：既有 5 条读语义须保留 | §4.1/§4.2 显式声明 + 回归用例 |
| revB P2：panic 隔离 / Query→Rotate 递归 / 归档↔轮转 / seal→open 窗口 | **E21–E24** |
| revB P2：`Clock` 缺 `Since` | §4.2 补 `Since` |
| revB P2：`Entry` 缺 `Level/Dim`（旧文件不可 round-trip） | §4.1 补齐 |
| revB P2：门禁误报 `var ErrNoSink/scopeIDPattern` | §9.2 白名单口径 |
| revA/revC：计数不符（~130/~35） | 实测订正：`RecordAudit` **115 处/20 文件** + `recordFileAudit` 29 处/3 文件 |
| revC P2：配置键重复/`buffer_size` 关系/阶段名 S3 歧义/E 编号 | 见 program.md 修订记录 |

### 13. 裁定记录（v3，2026-10-19 用户问答）

| 项 | 裁定 | 设计落点 |
|---|---|---|
| `AuditStore`/`AuditRing` 下沉方案 | **② 彻底**（类型迁入 `pkg/audit`） | M1-3（含 `Journal.Prune`/`Index.Evict` + 白盒测试迁移） |
| 中断继续集群语义 | **本期单机** | §2.11 / E9（跨节点恢复另立专题） |
| 获取路径收敛范围 | **两层一并收敛**（quota + capacity） | waitq.md §8.2/§7（`Group` 原子） |
| 审计读面权限 | **两维模型**（用户维度/系统维度；非管理员脱敏读自己；管理员全量；服务端强制） | **§5.7**（新增）+ M1-11 + E26 + R35-e + §9.1 用例 |
| 审计存储归属（2026-10-19 追问） | **保留系统 owner `__audit__`**（非管理员 owner；非审计账号）；多管理员靠**角色/能力位**而非所有权 ⇒ 可平滑扩展；新增红线 9（审计读也要审计）/10（归档卷不绑管理员）/11（逃生舱 `audit.archive_owner`） | **§5.8**（新增，含多管理员障碍分析） |
