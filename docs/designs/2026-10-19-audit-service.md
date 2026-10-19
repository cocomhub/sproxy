<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 统一审计服务（`pkg/audit`）—— 作用域化 / 内存活跃索引 + 轮转 / 结构体能力 / WAL 续跑

> 状态：**设计（待评审后实施）**。用户裁定（2026-10-19）：
> ① 审计能力**必须由 `audit` 包提供**，禁止到处实现（不收敛、难管理）；
> ② 必须**抽象**，**所有审计都有各自的作用域**；
> ③ **活跃记录在内存**加速访问；**内存超过配置大小 / 内容过时 / 人工读取等场景立刻触发轮转**；
> ④ 能力**必须由结构体提供**（禁全局变量）⇒ 可测试、不污染其他功能测试；
> ⑤ 生产可用后**必须补各业务功能在审计场景的测试 + 自动恢复能力**，锁定场景防回归；
> ⑥ 设计原则：**pkg 包提供通用能力抽象，使用方灵活扩展场景**。

---

## 1. 现状与问题（证据）

当前**两套**审计能力，**互补但割裂**：

| 维度 | A：`pkg/audit`（框架） | B：`pkg/server/audit*.go`（合规事件） |
|---|---|---|
| 语义 | 过程审计/诊断：步骤 span、资源、错误、跨步骤 kv | 合规审计：谁在何时对什么做了什么 + 结果 |
| 组织 | 按**作用域**：`<scopeID>.audit.log` 每 scope 一文件 | 按**全局时间线**：有界 ring + 单文件 + 轮转 |
| 持久化 | `FileSink`：JSON lines + **每行 fsync** + 0700 目录/拒软链/scopeID 白名单 | `AuditStore`：JSON lines + 轮转；**`_ = Append` 尽力而为**（吞错） |
| 读面 | `Recent(Filter)`（包内） | `ring.Recent` + `/api/audit` + `/api/audit/export` + Web UI |
| 扩展 | `Sink` 可插拔、`Archiver` **预留未接线** | `notifyCenter.Dispatch`（通知中心） |
| 状态 | 跨步骤 kv（`Save/Get`）**仅内存** | 无作用域/状态概念 |
| 消费者 | cloud 任务、secretdata（encrypt/decrypt span）、transfer | **~130 处** `RecordAudit` 散在 **~35 个文件** |

**结构性缺陷**（不是「两种风格」，是**能力错位**）：

- **P1-a**：合规事件**只有 `pkg/server` 能记** ⇒ `pkg/cloud`/`pkg/syncexec`/`pkg/volume` 要记必须依赖 server 或自造 ⇒ **不可能内聚复用**。
- **P1-b**：A 有 scope + fsync + 可插拔 Sink + 归档钩子（WAL 语义）但**没有合规事件形态**；B 有合规形态但**无作用域、无重放、无终态** ⇒ 同一件事的两半。
- **P1-c**：**覆盖缺口**（逐 handler 核对，`RecordAudit` = 0）：

  | 操作 | 审计 |
  |---|---|
  | `/api/backends/{type}/presign` + `/presign/complete` | **零**（安全敏感：服务端凭据签任意键） |
  | `/mkdir` `/rmdir`（`pkg/files/dirs.go`） | 零 |
  | `/api/trash/restore` `/api/trash/empty`（破坏性） | 零 |
  | `/api/volumes/user` 建/删 | 零 |
  | `/api/secrets` 建/删 | 零 |
  | `/api/archive`（归档/转存） | 零 |
  | `/upload`（**新文件成功**） | 仅 error 分支 + overwrite 分支（files 侧 action 分布：delete 13 / rename 9 / overwrite 3 / copy 2 / **upload 1** / move 1） |
  | S3 multipart complete、DAV MKCOL/PROPPATCH、`/api/tags` | 零 |
  | **同步（`pkg/syncexec`）全部写入** | 零（该包无任何审计调用点） |

- **P1-d**：可靠性/可观测——落盘失败与 ring 覆盖**无任何信号**（无 `audit_dropped` 计数、无 fail-closed 选项）；**无 request-id / 源 IP / volume / size / 耗时** 字段（多卷多副本下「哪个卷」不可见）。
- **P1-e**：**无覆盖门禁** ⇒ 新增路由靠人记得加（这正是上表漏掉的根因）。
- **P2-a**：轮转只按**文件大小**（`audit.max_size`），无「内存超限 / 内容过时 / 读取触发 / 显式触发」。
- **P2-b**：`Save/Get` 的 kv 只在内存 ⇒ 进程崩溃即丢 ⇒ **无法作为续跑锚点**。

---

## 2. 目标与设计原则

1. **唯一提供方**：审计能力只由 `pkg/audit` 提供；其他包（含 `pkg/server`）只做**装配 + 使用**。
2. **一切审计皆有作用域**（Scope）：请求、任务、后台作业、运维动作都对应一个 scope；无 scope 的审计**不允许**（编译期/门禁强制）。
3. **活跃记录在内存**（快速访问）+ **轮转**：内存占用超配置 / 内容过时 / **读取时**（人工触发）/ 显式调用 / scope 结束 / 进程退出 ⇒ 立即轮转。
4. **结构体能力，零全局变量**：`*audit.Service` 由装配层注入；测试自建 service + 内存后端 ⇒ 隔离、不污染其他功能测试。
5. **通用抽象 + 使用方扩展**：`Journal`/`Index`/`Notifier`/`Clock`/`Policy` 均为接口；使用方按需实现。
6. **一份日志两个用途**：审计事件与**任务状态（WAL）**同处一条 append-only 流 ⇒ 审计天然是恢复的证据链（见 §6）。
7. **不阻断业务**（默认 best-effort）+ **可选 fail-closed**（合规场景）；任何失败**可观测**。
8. **包提供全部能力，使用方按场景选择性启用**（用户裁定 2026-10-19）：**合规事件形态、过程 span、资源采样、WAL 状态、归档、索引查询、通知、加密存储、配额治理**均为**可选能力** ⇒ 使用方声明 `CapabilitySet`（§10 能力矩阵逐项标注「可选/默认」），包不做一刀切。
9. **审计落盘内容必须纳入配额治理**（防磁盘打满）：本地热段**预留额度**（系统关键路径，不与用户数据竞争）；卷内归档走**卷配额**（§5.4）。
10. **审计数据可归档到外部存储卷**（长期留存复用**卷 / 外部卷 / 加密卷**能力，本地只留热段 ⇒ 本地占用有上界；§5.5）。

---

## 3. 总体架构

```
                       ┌──────────────── pkg/audit（唯一审计包）────────────────┐
  使用方（任意包）      │  Service（结构体，注入式，无全局）                    │
  ────────────────     │   ├─ Scope：作用域句柄（Open/Close/MarkDone）         │
  h.Audit.Emit(...)    │   ├─ Span：Begin/End/Fail（过程审计）                 │
  audit.From(ctx)      │   ├─ Save/Get：作用域 kv（**落盘** = WAL）            │
                       │   ├─ Query/Resume/ListScopes：读/重放/未完成作用域     │
                       │   └─ Rotate(reason)：显式轮转                         │
                       │      │                                                │
                       │      ├─ Journal（持久化后端，append-only）             │
                       │      │    SegmentFileJournal：分段文件 + 轮转 + 归档   │
                       │      │    MemoryJournal：测试/临时                    │
                       │      ├─ Index（内存活跃索引，有界）                    │
                       │      ├─ Notifier（下游钩子：通知中心/告警）             │
                       │      └─ Clock（可注入 ⇒ 过时判定可测）                 │
                       └───────────────────────────────────────────────────────┘
装配层（pkg/server / cmd/sproxy）：构造 Service → 注入 Handlers/Manager/…；
                                 挂 auditMiddleware（传输层兜底）+ /api/audit 读面
```

**分层职责**：

| 层 | 职责 | 不做什么 |
|---|---|---|
| `pkg/audit` | 条目模型、作用域、内存索引、轮转策略、持久化抽象、重放、指标 | 不认识 HTTP、不认识业务动作语义 |
| 装配层（server/cmd） | 构造 Service、注入使用方、`auditMiddleware` 兜底、HTTP 读面/Web UI | 不自己拼审计条目 |
| 使用方（files/cloud/syncexec/…） | 发**语义**审计（action/object/volume/result/detail） | 不碰落盘/轮转/索引 |

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
    // 身份与顺序
    Scope ScopeID `json:"scope"`            // 必填（空 ⇒ Emit 拒绝，见 §7 边界 E1）
    Seq   uint64  `json:"seq"`              // 每 scope 单调；**权威顺序**（不用 TS 排序）
    Kind  Kind    `json:"kind"`
    // 合规语义
    Action string `json:"action,omitempty"` // 命名空间化：file.delete / volume.create / quota.set …
    Actor  string `json:"actor,omitempty"`
    Mesh   string `json:"mesh,omitempty"`
    ObjType string `json:"object_type,omitempty"`
    Object string `json:"object,omitempty"`
    Volume string `json:"volume,omitempty"` // 多卷/多副本可归属
    Result Result `json:"result,omitempty"` // success | denied | error | pending
    Detail string `json:"detail,omitempty"`
    // 过程/资源
    Step string `json:"step,omitempty"`
    StartMS, DurMS int64 `json:"start_ms,omitempty"` // …
    Bytes, BwBps   int64
    Err  string `json:"err,omitempty"`
    // 关联与上下文
    RequestID string `json:"request_id,omitempty"`
    SourceIP  string `json:"source_ip,omitempty"`
    Node      string `json:"node,omitempty"`
    TS        time.Time `json:"ts"`
    Meta      map[string]any `json:"meta,omitempty"`
}
```

**兼容**：`/api/audit` 的现有 JSON 契约（`action/actor/mesh/object_type/object/result/detail/ts`）**逐字段保留** ⇒ Web UI 零改动（新增字段为纯增量）。`pkg/audit` 现有 `Row` 的 `type/level/dim/step/start_ms/dur_ms/err/bw_bps/bytes/meta/node` 由 `Kind/Step/…` 映射保留 ⇒ `FileSink` 既有文件可被新读取器解析（**迁移期双读**，见 §8）。

### 4.2 提供方接口（使用方扩展点）

```go
// Journal：append-only 持久化（WAL）。实现必须保证「Append 成功 ⇒ 已 fsync」或显式报告未落盘。
type Journal interface {
    Append(e Entry) error
    Read(f Filter) ([]Entry, error)
    Segments() []SegmentInfo         // 段清单（含 seal 状态、大小、起止 TS/Seq）
    Rotate(reason string) error      // seal 当前段 + 开新段
    Close() error
}

// Index：内存活跃索引（有界；快速访问）。
type Index interface {
    Add(e Entry)
    Query(f Filter) []Entry          // 按 scope/actor/action/时间/结果过滤
    Len() int
    ApproxBytes() int64
    Evict(n int) []Entry             // 逐出（**必须已落盘**，见 §7 E4）
}

// Notifier：下游钩子（通知中心/告警/SIEM）。
type Notifier interface{ Notify(ctx context.Context, e Entry) }

// Clock：可注入 ⇒ 「内容过时」可确定性测试（禁 time.Sleep）。
type Clock interface{ Now() time.Time }
```

### 4.3 门面 API（结构体，注入式）

```go
type Options struct {
    Journal  Journal            // 必填（MemoryJournal 供测试）
    Index    Index              // 可选（nil = 无内存索引）
    Notifier Notifier           // 可选
    Clock    Clock              // 可选（默认 systemClock）
    Policy   Policy             // 轮转策略（§5）
    NodeID   string
    Metrics  Metrics            // 可选（丢弃/失败/等待计数）
}

func New(opts Options) (*Service, error)        // 无包级变量；并发安全

func (s *Service) Open(ctx context.Context, id ScopeID, meta ScopeMeta) (*Scope, error)
func (s *Service) Emit(ctx context.Context, e Entry) error         // 合规事件
func (s *Service) Begin(ctx context.Context, step string, kv ...any) *Span
func (s *Service) Save(ctx context.Context, k string, v any) error // WAL：**落盘**
func (s *Service) Get(ctx context.Context, k string) (any, bool)
func (s *Service) Query(ctx context.Context, f Filter) ([]Entry, error)  // 读取 ⇒ 可触发轮转（§5.3）
func (s *Service) Resume(ctx context.Context, id ScopeID) (*ScopeState, error)
func (s *Service) ListScopes(ctx context.Context, state ScopeStateFilter) ([]ScopeInfo, error)
func (s *Service) MarkDone(ctx context.Context, id ScopeID) error
func (s *Service) Rotate(ctx context.Context, reason string) error
func (s *Service) Close() error
```

`WithContext/From(ctx)` 保留（ctx 传递 scope 句柄）；`From` 返回 `*Scope`，**nil 安全**（无 scope ⇒ `Emit` 报错或按策略降级，见 §7 E1）。

---

## 5. 轮转、落盘与存储策略（用户要求③的落地）

### 5.1 触发条件（`Policy` 显式配置，全部可注入 Clock 测试）

| # | 触发 | 配置键 | 说明 |
|---|---|---|---|
| T1 | **内存占用超限** | `audit.max_memory_bytes` / `audit.max_buffer_entries` | 活跃索引超阈值 ⇒ flush + Rotate |
| T2 | **内容过时** | `audit.max_age`（如 `5m`） | 最早未落盘条目超龄 ⇒ Rotate（后台 ticker 或惰性判定，二选一，见 §7 E6） |
| T3 | **读取触发（人工）** | `audit.rotate_on_read`（默认 true）+ `min_rotate_interval` | `Query`/`/api/audit`/`/api/audit/export` 发现「脏且超限/超时」⇒ **先轮转再返回** |
| T4 | **显式调用** | — | `Service.Rotate(reason)`（运维/信号/SIGHUP） |
| T5 | **作用域结束** | — | `Scope.Close()`/`MarkDone` ⇒ flush + seal |
| T6 | **进程退出** | — | `Service.Close()`（`cmd/sproxy` 停服链） |
| T7 | **段大小超限** | `audit.max_size` / `audit.max_archives` | 既有行为（保留，语义不变） |

### 5.2 落盘策略：**包提供策略族，使用方按场景选择**（不硬编码）

| 策略（包提供） | 语义 | 适用场景（使用方选择） |
|---|---|---|
| `SyncAlways` | 每行 fsync | 合规证据、WAL 状态（崩溃丢行不可接受） |
| `SyncBatch` | 批量 + 定时/阈值 flush（可配 `batch_max_lines` / `batch_max_interval`） | 诊断 span、归档单元 |
| `SyncNone` | 仅缓冲（依赖轮转/退出时 flush） | 高频资源采样（可整体关闭） |
| `Memory` | 只进内存索引，不落盘 | 纯 UI 回看场景（用户显式选择，明确放弃持久化） |

**选择粒度**：按 `Kind` **或**按 scope **或**按使用方（`CapabilitySet`）三者可覆盖（优先级：scope > Kind > 全局默认）；包只提供实现，**默认映射**建议 `event/done/state=SyncAlways`、`span/rotate=SyncBatch`、`resource=SyncBatch`——使用方可逐项改写。

### 5.3 轮转语义

- **seal 当前段 → 开新段**（段 = 文件），归档保留 `max_archives` 份（既有语义）；
- 轮转本身写一条 `KindRotate` 条目（`reason` ∈ `memory_limit|age|read|manual|scope_end|shutdown|size`）⇒ **审计的审计**；
- 轮转期间 **Emit 不丢**：段切换在 `Service.mu` 内完成（或双段写 + 原子换指针），见 §7 E2；
- **读取触发防抖**：`min_rotate_interval` 内不重复触发（防「频繁读 ⇒ 频繁轮转」）。

### 5.4 存储归属与配额治理（用户裁定：审计落盘必须纳入配额，避免磁盘打满）

**问题**：审计日志若无上限，会与用户数据争磁盘；磁盘满时审计写失败 ⇒ 审计链断裂（合规事故），且 `fail_closed` 会把「磁盘满」放大成「全部业务失败」。

**方案（分层）**：

| 层 | 位置 | 配额机制 | 说明 |
|---|---|---|---|
| **热段（本地）** | `audit.persist_dir`（0700） | **预留额度** `audit.local_reserve_bytes`（默认 256 MiB）+ 硬上限 `audit.max_total_bytes` | 系统关键路径 ⇒ **预留**（不与用户数据竞争）；超预留 ⇒ **触发轮转/归档**（**不是等待**） |
| **冷段（卷内归档）** | 保留用户在指定卷下的路径（§5.6） | 走**卷 / 用户配额**（`quota` 的 owner scope，与用户数据同机制） | 超限按策略（丢最旧归档 / 停止归档 + 告警 / 请求扩容） |

**与 `pkg/waitq` 的关系**：热段预留与冷段归档额度都注册为 `waitq.Resource`（`audit.local` / `audit.archive`）⇒ **复用 W1 通用等待能力**（指标/背压/优先级）。热段**不等待**（超限即轮转/归档 ⇒ 用 `ErrImpossible` 语义），冷段可配置是否等待。

**为何热段要预留而非竞争**：审计是**证据链** ⇒ 不能因用户写满磁盘而写不进去。预留额度 = 「无论业务如何，审计总有 X MiB 可写」；达到预留 ⇒ 立即归档回收（§5.5）。

### 5.5 归档到外部存储卷（用户裁定：后续规划支持）

**目标**：本地只留热段 ⇒ 本地占用有上界；长期留存**复用**卷 / 外部卷 / 加密卷能力（不新增存储机制）。

**流程**：

1. 热段 sealed（轮转 T1–T7 任一）⇒ 形成**归档单元**（段文件 + 摘要：起止 TS/Seq/条目数/校验和）；
2. 经**文件写内核**（统一写入口）写入保留用户路径（§5.6）⇒ 自动获得**配额记账 / 加密卷透明加解密 / 版本 / 回收站 / 多卷**；
3. 归档成功 ⇒ 热段按保留策略删除（`audit.local_keep_segments`）；失败 ⇒ 保留热段 + 重试 + 告警（**绝不静默丢**）；
4. 归档动作本身写审计条目（`action=audit.archive`，含卷/路径/字节/校验和）⇒ **审计的审计**。

**读取**：`Query` 先查内存索引 → 热段 → 需要时归档段（按摘要定位 ⇒ 只读相关段，不扫全量）。
**加密**：归档到加密卷 ⇒ at-rest 加密（复用 `extra.encrypt`）；本地热段为明文（0700 目录，与现状一致）。

### 5.6 审计数据的保留用户（系统用户）—— 推荐方案

**你的问题**：是否推荐以**内部预留的审计用户 / 系统用户**管理审计数据（可复用外部卷、加密卷能力）？

**推荐：是（保留 owner + 混合存储）**。理由与边界：

| 维度 | 保留 owner 方案（推荐） | 纯本地目录方案（现状） |
|---|---|---|
| 配额记账 | **复用**（owner scope + 卷容量池） | 需自造（§5.4 预留额度） |
| 加密 | **复用**（加密卷透明加解密） | 需自造 |
| 外部卷 / 多卷 | **复用**（归档到任意卷） | ✗ |
| 版本 / 回收站 / 生命周期 | **复用** | ✗ |
| 统一恢复（WAL） | **复用**（归档是普通写 ⇒ 可续跑） | ✗ |
| DAV/S3/Web 可见性 | 需**显式隐藏/只读**（安全红线） | 天然不可见 |
| 性能 | 走写内核（比 append 慢）⇒ **只用于冷段归档** | 快（热段） |
| 自举风险 | 配额不足 ⇒ 归档失败 ⇒ 需热段兜底 + 告警 | 无 |

**保留 owner 命名（待裁定 O6）**：建议 `__audit__`（`LogicalKey.Owner` 为字符串，可承载；与用户 owner 空间不冲突）。

**安全红线（实现时锁死）**：

1. **普通用户不可读 / 写 / 删**该保留 owner 的路径（文件面 / DAV / S3 / 分享 / 回收站全部拒绝）——审计数据可被篡改即失效；
2. **不在 Web UI 文件列表**展示（或仅管理员「审计存储」页）；
3. **不可通过分享 / 直链外发**；
4. 保留 owner 的**配额**单独设定（建议不受用户配额回收影响）；
5. 该 owner 的写操作**不得再产生审计条目**（防递归：审计写入标记 `Meta["no_audit"]=true`，见 E16）。

**为何不选纯本地或纯卷内**：纯本地 ⇒ 本地占用无上界、无配额/加密/外部卷能力；纯卷内 ⇒ 热路径依赖写内核（延迟↑）+ 自举风险（配额不足写不进审计）。⇒ **混合**（热段本地 append-only + 冷段归档到保留用户卷内路径）同时满足**低延迟、崩溃安全、配额治理、加密、外部卷归档**。

---

## 6. WAL 与「中断继续」（最终目标）

**同一份日志承载两类事实**：合规事件（谁做了什么）+ 任务状态（进度/断点）⇒ 恢复时**重放同一日志**即可续跑，且**审计链天然完整**（恢复动作本身也是审计条目）。

### 6.1 作用域状态机

```
Open(scopeID) ──▶ active ──(Save 落盘 KindState，带 Seq)──▶ active
                    │
                    ├──(异常/崩溃)──▶ 重启扫描 ⇒ unfinished（末行非 KindDone）
                    └──(MarkDone)──▶ done（KindDone 落盘 ⇒ 恢复时跳过）
```

### 6.2 恢复原语

| 原语 | 语义 |
|---|---|
| `ListScopes(unfinished)` | 扫段目录 ⇒ 未完成作用域清单（含最后 Seq/TS/进度摘要） |
| `Resume(scopeID)` | 按 **Seq 顺序**重放 `KindState` ⇒ 重建状态（**幂等**：同 Seq 重复应用无副作用） |
| `MarkDone(scopeID)` | 写终态 ⇒ 之后 Resume 直接跳过（不重复执行已完成任务） |
| 租约（`Lease`） | `Open(scopeID)` 可带 lease（owner+TTL）⇒ **多节点单写者**，防重复续跑 |

### 6.3 与 M3 统一恢复协调器的关系

`docs/archive/architecture-task-recovery.md`（用户 2026-10-10 裁定）要求**统一恢复协调器**收敛 5 处独立实现（cloud `recoverTasks` / sync `recoverTasks` / chunked `recoverSessions` / baidupcs 断点未接线 / 下载 `.partial` 仅整文件哈希）。本设计的 `ListScopes/Resume` 正是**协调器的注册与重放原语** ⇒ M3 = 在 `pkg/audit` 的 WAL 之上建**协调器 + 注册中心**（而不是各管理器各扫各的目录）。

---

## 7. 边界场景（必须实现时逐条覆盖）

| # | 场景 | 处理 |
|---|---|---|
| E1 | **无作用域的审计** | `Emit` 拒绝并返回错误（+ 计数器）；装配层 `auditMiddleware` 为每个请求开 scope ⇒ 业务侧几乎不可能无 scope |
| E2 | **轮转与并发 Emit** | 段切换持 `Service.mu`；`Journal.Rotate` 保证「切换期间 Append 不丢」（seal 后到新段打开之间的行缓冲到新段） |
| E3 | **fsync 失败**（磁盘满/只读/EIO） | 默认：**保留内存副本**（不丢）+ `audit_dropped`/`audit_write_failed` 计数 + 告警 + 重试；`audit.fail_closed=true` ⇒ 返回错误给调用方（合规场景可选） |
| E4 | **内存逐出 vs 未落盘** | `Evict` **只逐出已落盘条目**（`Journal.Append` 成功后才标记 durable）⇒ 内存超限时**先落盘再逐出**，绝不因内存压力丢审计 |
| E5 | **崩溃后半行/坏行** | 读取时丢弃**不完整尾行**与解析失败行（`FileSink.Recent` 已跳过解析失败行；**新增**尾行截断判定 + `audit_corrupt_lines` 计数） |
| E6 | **T2 过时轮转的实现** | 无后台 goroutine 版本：在 `Emit/Query/Rotate` 时惰性判定（省 goroutine、可注入 Clock 测试）；如需严格时效再引入 ticker（装配层可选） |
| E7 | **时钟回拨/并发 TS 相同** | 顺序权威 = `Seq`（不用 TS 排序）；TS 仅作展示 |
| E8 | **scopeID 路径安全** | 沿用 `FileSink` 白名单（字母数字开头 + `[A-Za-z0-9._-]{0,128}`）+ 0700 私有目录 + 拒软链 |
| E9 | **多节点同 scope** | `NodeID` 标注 + `Lease` 防重复续跑；冲突时后到者失败并审计 |
| E10 | **敏感内容** | **禁止**把密钥/凭据/SK 写进 `Entry`（`RecordAudit` 现状已声明）⇒ 加**测试锁定**（对已知敏感字段名做扫描断言） |
| E11 | **读取即轮转抖动** | `min_rotate_interval` 冷却 |
| E12 | **审计写入阻塞业务** | 默认 best-effort（异步/带超时）；`SyncAlways` 仅用于合规事件（单行 fsync 量级可接受） |
| E13 | **禁用态** | `audit` 未配置 ⇒ no-op Service（`Emit` 静默成功 + 计数器），**零回归** |
| E14 | **迁移期双格式** | 旧 `AuditStore` 文件（`AuditEvent` JSON）与新 `Entry` JSON **可并存读取**（`Filter` 适配），滚动迁移后再删旧读路径 |
| E15 | **容量/轮转上限** | 段总数 + 总字节上限（`audit.max_total_bytes`）⇒ 防审计日志吃满磁盘（与配额/staging 同思路） |
| E16 | **审计写入自身产生审计（递归）** | 审计/归档写入标记 `Meta["no_audit"]=true` ⇒ 写内核跳过审计；门禁断言该标记存在 |
| E17 | **审计配额不足 / 自举兜底** | 热段达预留 ⇒ 先归档回收；归档也失败 ⇒ **保留热段 + 降级为 Memory 策略 + 告警**（绝不静默丢、绝不阻断业务） |
| E18 | **保留 owner 被用户触碰** | 文件面/DAV/S3/分享/回收站一律拒绝（403）+ 审计一条 `action=audit.retained_owner_violation` |
| E19 | **归档部分成功 / 校验和不符** | 归档单元带校验和；不符 ⇒ 视为失败（保留热段 + 重试 + 告警），**不得删除热段** |
| E20 | **热段被外部删除 / 截断** | 启动与轮转时校验段连续性（Seq 缺口/摘要不符）⇒ 记 `audit_corrupt_segments` 计数 + 告警，不阻断启动 |

---

## 8. 迁移与门禁

### 8.1 迁移步骤（逐步，不破坏）

| 步骤 | 内容 | 验证 |
|---|---|---|
| M1-1 | `pkg/audit` 增 `Entry/Kind/Result/Journal/Index/Notifier/Service`（**纯新增**，旧 `Logger/Row/FileSink` 保留） | 新包单测 ✓ 旧测试全绿 |
| M1-2 | `pkg/server.RecordAudit` **改为薄委托** `h.audit.Emit(...)`（行为逐字段等价） | `pkg/server/audit_*_test.go` 全绿（863+519+…） |
| M1-3 | `AuditRing`/`AuditStore` 实现下沉为 `pkg/audit` 的 `Index`/`SegmentFileJournal`（`pkg/server` 保留薄适配与装配） | 现有 ring/store/rotation 用例**原地通过** |
| M1-4 | **覆盖缺口补齐**（§1 P1-c 全表）+ `auditMiddleware` 传输层兜底 | 新增业务审计用例（§9） |
| M1-5 | 结构化字段（volume/request_id/source_ip/size/dur）+ 丢弃/失败计数 + `fail_closed` 选项 | 用例 + 指标断言 |
| M1-6 | 轮转四触发（T1/T2/T3/T4）+ 读取即轮转 | 可注入 Clock 的确定性用例 |
| M1-7 | 删旧散点（棘轮收敛） | 门禁 R35 通过 |
| M1-8 | **存储配额治理**：本地热段预留（`audit.local` Resource，复用 W1 `waitq`）+ `max_total_bytes` 硬上限 + 超限即轮转/归档 | 配额用例（预留不被用户占用、超限触发归档） |
| M1-9 | **保留用户（§5.6）**：保留 owner 装配 + 安全红线 5 条 + 防递归标记 | 越权用例（用户读/写/删/分享/回收站全拒 403） |
| M1-10 | **能力选择性启用**：`CapabilitySet`（合规形态等为可选）+ 落盘策略族选择 | 各使用方声明生效断言 |
| **M4（后续阶段）** | **归档到外部/加密卷**（§5.5）：sealed 段 → 写内核 → 保留用户路径 → 热段回收；`Query` 跨热/冷段 | 归档往返用例（含加密卷、外部卷）+ 归档失败不丢热段 |

### 8.2 门禁（结构门禁 > 运行时用例，沿用既有纪律）

| 门禁 | 断言 | 理由 |
|---|---|---|
| **R35-a** | 非测试源码中 `RecordAudit`/`Emit` 的调用点**必须在登记表内**（棘轮：现有散点登记后**只减不增**） | 防「又到处实现」 |
| **R35-b** | **所有变更型路由**（POST/PUT/PATCH/DELETE）必须经 `auditMiddleware`（或登记豁免 + 理由） | 覆盖完备性不依赖人 |
| **R35-c** | `pkg/server` **不得**再定义审计条目类型/落盘实现（只能装配） | 唯一提供方 |
| **R37**（M2 后） | 长任务（cloud/sync/chunked/backup/transfer）必须 `Open` 作用域（可续跑） | 中断继续前提 |

---

## 9. 测试计划（用户要求⑤：生产可用后必补，锁定防回归）

### 9.1 审计场景用例（按业务功能，逐项断言 action/actor/object/volume/result）

| 业务 | 用例断言 |
|---|---|
| 文件写面 | upload（**新增**）/ overwrite / rename / copy / move / delete / soft-delete（含 denied/error 分支） |
| 目录 | mkdir / rmdir（含配额不足 denied） |
| 归档 | archive（含 target volume） |
| 回收站 | restore / empty（`all=1` 与单卷） |
| 卷 | create/delete user volume、move/rebalance/copy/import、quota set |
| 密钥 | secrets create/delete（**断言不含密钥明文** E10） |
| **presign** | presign + presign/complete（含被拒） |
| 版本 | restore / delete |
| 云任务 | cancel / delete / archive |
| 同步 | syncexec 拉取/推送（**新增覆盖**） |
| 传输层 | 任意变更路由 ⇒ 至少 1 条 `KindEvent`（含 request_id/source_ip） |

### 9.2 恢复与可靠性用例

- **崩溃重放**：写 scope 状态 → 模拟崩溃（不 `MarkDone`）→ 新 Service 实例 → `ListScopes(unfinished)` 命中 → `Resume` 重建状态 ✓；
- **幂等**：同一段重放两次 ⇒ 状态一致、无重复副作用 ✓；
- **已完成跳过**：`MarkDone` 后 `Resume` 不执行 ✓；
- **轮转不丢**：并发 Emit + 同时 T1/T3 触发 ⇒ 条目总数 = Emit 数（无丢失）✓；
- **内存压力**：`Index` 超限 ⇒ 先落盘再逐出，落盘条目数不减 ✓；
- **写入失败**：注入失败 Journal ⇒ 业务不受影响（默认）+ `fail_closed=true` 时业务被拒 ✓；
- **隔离性**（要求④）：两个测试各自 `New()` ⇒ 互不影响（**无包级变量**断言：门禁扫描 `pkg/audit` 禁包级可变状态）✓。

### 9.3 测试纪律

`t.Parallel()` 默认 ✓；**禁新增 `time.Sleep`**（R14 棘轮）⇒ 过时判定用注入 Clock ✓；serial 预算棘轮 ✓；每包必须有测试 ✓。

---

## 10. 能力矩阵（审计）

| 能力 | 现状 | 目标 | 归属 |
|---|---|---|---|
| 合规事件（谁/什么/结果） | server 私有 | `Service.Emit` | 全包可用 |
| **作用域（强制）** | 仅 A / B 无 | `Open(scopeID)` | ✓ |
| 内存活跃索引（快速访问） | B ring（无 scope） | `Index`（scope/actor/action/时间/结果） | ✓ |
| 轮转：内存超限 | 无 | T1 | ✓ |
| 轮转：内容过时 | 无 | T2 | ✓ |
| 轮转：**读取触发** | 无 | T3 | ✓ |
| 轮转：显式/结束/退出/大小 | 部分（仅大小） | T4–T7 | ✓ |
| 落盘策略（每行 fsync / 批量） | A 恒 fsync / B 尽力 | 按 Kind 可配 | ✓ |
| 归档到卷 | `Archiver` 预留 | 接线（M2） | ✓ |
| WAL：Save 落盘 / Seq / Resume / ListScopes / MarkDone / Lease | 无 | §6 | ✓ |
| 传输层兜底（覆盖完备） | 无 | `auditMiddleware` | 装配层 |
| 结构化字段（volume/reqID/IP/size/dur） | 部分 | 统一 | ✓ |
| 丢弃/失败可观测 + fail-closed | 无 | `Metrics` + 选项 | ✓ |
| 无全局变量 / 可注入 Clock/后端 | 混用 | 全结构体 | ✓ |
| 通知/告警下游 | server 内 | `Notifier` 接口 | ✓ |
| 敏感内容禁写 | 声明 | 测试锁定 | ✓ |
| **能力选择性启用**（CapabilitySet） | 无（一刀切） | 使用方按场景声明（合规形态=可选） | ✓ |
| **存储配额治理**（本地预留 + 卷配额） | 无 | §5.4（注册为 `waitq.Resource`） | ✓ |
| **归档到外部 / 加密卷** | `Archiver` 预留 | §5.5（走写内核 ⇒ 复用配额/加密/版本/多卷） | ✓ |
| **保留用户（系统用户）数据管理** | 无 | §5.6（复用外部卷/加密卷；安全红线 5 条） | ✓ |

---

## 11. 风险与开放问题

| # | 项 | 说明 / 建议 |
|---|---|---|
| ~~O1~~ | ~~fsync 粒度~~ | **已裁定（2026-10-19）**：包提供策略族（`SyncAlways`/`SyncBatch`/`SyncNone`/`Memory`），**使用方按场景选择**（§5.2） |
| ~~O2~~ | ~~`fail_closed` 默认~~ | **已裁定**：默认 best-effort（零回归），合规部署显式开启 |
| O6 | 保留 owner 命名 | 建议 `__audit__`（§5.6）；如你偏好 `_system`/`__system__` 请指定 |
| O7 | 本地热段预留额度默认值 | 建议 256 MiB（`audit.local_reserve_bytes`）+ 硬上限默认 0=不限（部署显式设） |
| O8 | 归档触发阈值 | 建议按「热段总量 > 预留的 70%」或「sealed 段数 > N」双阈值（可配） |
| O3 | T2 过时轮转是否需后台 ticker | 建议惰性判定（无 goroutine、可测）；若要求严格时效再加 ticker |
| O4 | 旧 `AuditStore` 文件是否长期双读 | 建议迁移期双读（E14），一个版本后删旧读路径 |
| O5 | 段文件是否按 scope 还是全局 | 建议：**合规事件 = 全局段**（时间线，便于合规导出）；**任务/WAL = 按 scope 段**（便于 Resume）⇒ `Journal` 支持两种布局（`Layout` 选项） |
