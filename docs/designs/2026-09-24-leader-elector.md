# LeaderElector 选主（集群化方案 A 配套）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：`pkg/state` 包内新增文件（与 StateStore 同包共存）
> 关联设计：[2026-09-24-statestore.md](./2026-09-24-statestore.md)

## 1. 背景与目标

### 1.1 问题

sproxy 现状无选主（leader/raft grep 空）。多节点挂同一外部卷时：
- 每个节点各自写本地 meta JSON（checksum/dedup/分享/配额），静默互相覆盖；
- 配额双账本、dedup 引用计数在跨进程下没有写面仲裁；
- 索引失效、凭据登记等「写面副作用」无唯一执行者。

目标：**写面节点唯一**——多节点挂同一外部卷时，仅主节点可写；从节点可读（文件本体只读服务）但拒绝写请求（或转发给主——转发不在本期范围）。

### 1.2 目标

- `LeaderElector` 接口：`TryAcquire(ctx, leaseID, ttl) (bool, error)` / `Renew(ctx, leaseID) error` / `Release(ctx, leaseID) error`；
- `LocalLeaderElector`：`<root>/state/leader.lock` flock 排他——单节点恒主、**零回归**（本地 flock 不改变单节点行为）；
- `MongoLeaderElector`：`_id=leader` 文档 + TTL index 租约续租（分布式）；
- 与 StateStore 同包 `pkg/state/`（它们共享 key 规范、注册表与配置段）；
- 门禁：禁非测试源码直接 flock（引导走 LeaderElector 抽象）。

### 1.3 非目标

- 不做故障自动切换的完整 raft 共识（那由 RaftStateStore 承担，本期不实现）；
- 不做「从节点转发写请求到主节点」的数据面（写面唯一化的网络转发属集群方案 A 的后续片）；
- 不做租约精度保证（时钟偏移安全由 TTL/续租周期设计消化，见 §4）；
- 不管理多主脑裂后的数据修复（文档明示运维流程：以主节点 meta 为准重建从节点）。

## 2. 组件与接口

### 2.1 核心接口（任务给定形状）

```go
// LeaderElector 是选主抽象：写面唯一化的互斥租约。
// 实现约定：
//   - TryAcquire：尝试获得租约；成功返回 (true, nil)；已被他人持有返回 (false, nil)（非错误）；
//     ttl <= 0 → 拒绝（调用方必须显式给 ttl，防「无限期持有」的隐式语义）；
//   - Renew：续租本 leaseID 持有的租约；未持有/租约已过期被他人接管 → 返回 ErrLeaseLost；
//   - Release：释放本 leaseID 持有的租约（幂等；未持有 no-op 成功）；
//   - leaseID 是调用方身份（如 node_id + 启动随机后缀）；空 → 拒绝。
type LeaderElector interface {
    TryAcquire(ctx context.Context, leaseID string, ttl time.Duration) (bool, error)
    Renew(ctx context.Context, leaseID string) error
    Release(ctx context.Context, leaseID string) error
}

// ErrLeaseLost 是续租失败（租约已过期被他人接管）的哨兵错误：
// 调用方必须停止一切写面操作并降级为只读（fail-closed）。
var ErrLeaseLost = errors.New("state: lease lost")
```

### 2.2 LocalLeaderElector

```go
// LocalLeaderElector 是本地 flock 实现：<root>/state/leader.lock。
// 语义：
//   - TryAcquire：O_CREATE|O_WRONLY 打开 + syscall.Flock(LOCK_EX|LOCK_NB)；
//     成功 → 写 leaseID + 时间戳 → (true, nil)；占用中 → (false, nil)；
//   - Renew：flock(LOCK_EX) 非阻塞校验本进程仍持有 → 更新时间戳 → nil；
//   - Release：Flock(LOCK_UN) + 关句柄（幂等）。
// 单节点恒主零回归：无竞争者时 TryAcquire 恒 true，与「无选主」行为一致。
type LocalLeaderElector struct {
    path   string   // <root>/state/leader.lock
    f      *os.File // 持有中的 flock 句柄（nil = 未持有）
    mu     sync.Mutex
}
```

要点：
- **Windows 兼容（已决策：LockFileEx 实现）**：`syscall.Flock` 在 Windows 不可用——用 `LockFileEx`（`golang.org/x/sys/windows`，`LOCKFILE_EXCLUSIVE_LOCK`）实现等价排他；Unix 用 flock。双平台 build tag 文件（`leader_unix.go` / `leader_windows.go`）共享接口，行为一致（TryAcquire/Renew/Release）。**不做「回落恒主」**——多节点部署 Windows 同样走 local 后端时正确互斥（需同目录文件系统）。
- 文件放 `state/` 目录而非 `<root>/leader.lock`：与 StateStore 目录统一、`state/` 由 StateStore 所有权管理（含清理语义）；
- 崩溃恢复：进程崩溃后 flock 由内核自动释放（无需清理）；写内容仅诊断用途。

### 2.3 MongoLeaderElector

```go
// MongoLeaderElector 用 <collection> 中 _id="leader" 文档做租约：
//   {_id: "leader", holder: <leaseID>, expires_at: <TTL index 到期时间>}
// 流程：
//   - TryAcquire：findOneAndUpdate({_id:"leader", $or: [不存在, holder=leaseID, expires_at < now]},
//       {$set: {holder, expires_at: now+ttl}}, upsert) → matched=1 → (true, nil)；否则 (false, nil)；
//   - Renew：findOneAndUpdate({_id:"leader", holder=leaseID}, {$set: {expires_at: now+ttl}}) →
//       未命中 → ErrLeaseLost；
//   - Release：findOneAndDelete({_id:"leader", holder=leaseID})（幂等）。
type MongoLeaderElector struct { ... }
```

要点：
- TTL index（`expiresAt` 字段 + `expireAfterSeconds: 0`）：文档到期的兜底清理（进程崩溃后租约自动过期）；
- **时钟偏移**：`expires_at` 由持有者写入（本地时钟），续租周期取 `ttl/3`；其它节点判断过期用**自己的时钟**比对 `expires_at`——偏移超 `ttl - 续租周期` 时可能双主，缓解：ttl 默认 30s、续租 10s、时钟偏移容忍 ±10s（文档明示；精确时钟同步不属于本组件职责）；
- 续租必须由持有者**主动循环**调用（装配层提供 `RenewLoop` helper：ticker ttl/3 + 退避重连，见 §3.4）；
- 防「旧主复活抢占」：新主 TryAcquire 后旧主 Renew 必失败（holder 已变）→ ErrLeaseLost → 旧主降级只读。

### 2.4 注册表与装配

- 复用 `pkg/state` 的注册表模式：`RegisterLeaderElector(name, factory)` / `NewLeaderElector(name, cfg, logger)`（仿 `RegisterStateStore`，同文件 `registry.go`）；
- 默认注册 `local`；`mongo` 与 `MongoStateStore` 共用同一连接配置（`state_store.mongo.*`）；
- 配置并入 `state_store` 段：

```yaml
state_store:
  type: local
  leader:
    enabled: true        # 默认 true（单节点 local 恒主零回归）；false = 明确关闭写面门
    lease_ttl: 30s       # 默认 30s；local 实现忽略 ttl（flock 语义）
    renew_interval: 10s  # 默认 ttl/3；local 实现忽略
```

### 2.5 写面装配（LeaderElector 消费方）

```go
// WriteGuard 是写面门：主节点放行、从节点拒绝。
type WriteGuard struct {
    elector LeaderElector
    leaseID string
    // isLeader 由 RenewLoop 持续维护（原子 bool）。
    isLeader atomic.Bool
}

func (g *WriteGuard) Authorize() error {
    if !g.isLeader.Load() { return ErrNotLeader } // 503 Service Unavailable + Retry-After
    return nil
}
```

装配点（`pkg/server` 的 `RegisterRoutes` / `cmd/sproxy/root.go`）：
- `POST /upload`、`POST /delete`、`POST /rename`、`POST /mkdir`、`POST /rmdir`、分块族（init/chunk/complete）、`POST /api/share`（revoke 也写）、`POST /api/versions/restore`、`PUT /api/config`、凭据写端点（register/renew/rotate）、cloud 下载任务创建 → 全部写面 handler 在业务逻辑前调 `WriteGuard.Authorize()`；
- 读面（GET/HEAD/list/search/stat/download/audit 查询/分享访问）**不设门**（从节点继续只读服务）；
- 从节点写请求回 `503 {"success":false,"message":"当前节点非主节点"}` + `Retry-After: 1`（客户端可重试/重定向到主）；
- `leader` 段 `enabled: false` → WriteGuard 不装配（`nil` = 旧行为零回归，单节点无门）。

## 3. 数据流

### 3.1 单节点（默认，local 后端）

```
启动 → NewLocalLeaderElector → TryAcquire → 恒 true
     → WriteGuard 装配 → isLeader=true
     → RenewLoop（本地无意义，仅校验进程存活：flock 校验 + 跳过时间戳写）
     → 所有写请求过 Authorize() → 恒放行（零回归）
     → 停服 → Release（flock 释放；进程崩溃内核自动释放）
```

### 3.2 多节点（mongo 后端）

```
节点 A、B 同时启动：
  A.TryAcquire → findOneAndUpdate upsert 成功 → (true, nil) → 写面放行
  B.TryAcquire → holder=A 且未过期 → (false, nil) → 写面 503
  A.RenewLoop（每 10s）：Renew → expires_at 顺延 → 持续持有
  A 崩溃（无 Release）→ TTL 到期 → mongo TTL 清理 / 竞态者按 expires_at<now 抢占
  B.TryAcquire 重试成功 → (true, nil) → B 成为主，写面放行
  A 复活 → A.Renew → ErrLeaseLost → A 降级只读（isLeader=false，后续写 503）
```

### 3.3 从节点读面

从节点仍可读（文件本体 + 只读 meta 快照）：`GET /download`、`GET /api/files` 等不设门。**一致性**：从节点读到的是「最后一次同步/挂载时的快照」，不保证实时（文档明示；实时读一致性由集群方案 A 的后续片承担）。

### 3.4 RenewLoop 语义

```go
// RenewLoop 是续租守护循环（装配层调用，goroutine）。
// 语义：每 renewInterval 调 Renew；ErrLeaseLost → isLeader=false + 日志 + 按退避
//   （1s→2s→4s→封顶 10s）重试 TryAcquire；成功 → isLeader=true。ctx 取消退出。
func (g *WriteGuard) RenewLoop(ctx context.Context) { ... }
```

- 退避重试使「主故障 → 从节点补位」自动化（TTL 窗口内完成）；
- 日志可观测：每次状态翻转（leader ↔ follower）Info 日志 + metrics（`sproxy_leader_active{node}` gauge）——安全开关可观测铁律。

## 4. 错误处理

| 场景 | 处理 | 语义 |
|---|---|---|
| TryAcquire 被持有 | `(false, nil)` | 非错误；调用方进入 follower 态 |
| Renew 租约丢失 | `ErrLeaseLost` | 调用方立即降级只读（fail-closed，禁「继续写」的宽限窗口） |
| mongo 不可达 | 返回 error | RenewLoop 退避重连；期间 isLeader 保持上一次状态（**安全方向**：网络分区时旧主按「续租失败 → 让位」处理——把分区收敛到新主，防双主；文档明示该取舍） |
| flock 失败（本地，Unix） | TryAcquire 返回 error | 装配期失败（启动报错）；运行期失败 → 写面 503 |
| Windows + local 后端 | 回落恒主 + 启动 Warn | 文档明示：Windows 本地部署单节点才安全；多节点必须 mongo |
| leaseID 为空 / ttl<=0 | 参数校验错误 | fail-fast（装配期） |
| 写请求打在从节点 | `ErrNotLeader` → 503 + Retry-After | 客户端可重试；数据面转发不在本期 |

## 5. 测试 + 变异点

### 5.1 单元测试（`pkg/state/leader_test.go`）

- `TestLocalLeaderElector_AcquireRelease`：TryAcquire 成功 → Renew 成功 → Release → 可再 Acquire；
- `TestLocalLeaderElector_SecondAcquireFails`：两个实例（同 path 两个 t.TempDir 不行——同目录两个句柄）同目录：A 持有 → B TryAcquire `(false, nil)`；A Release → B 成功（**核心互斥语义**）；
- `TestLocalLeaderElector_ReleaseIdempotent`：重复 Release 不报错；
- `TestMongoLeaderElector_TryAcquire`（mongo_integration build tag）：A 持有 → B 失败 → A Release → B 成功；
- `TestMongoLeaderElector_LeaseExpiry`：短 ttl（如 2s）→ 不续租 → 等待过期 → B 可抢占（条件轮询 `testutil.WaitFor`，不 time.Sleep——R14）；
- `TestMongoLeaderElector_RenewLost`：A 持有 → B 抢占（篡改 holder 或等过期）→ A Renew → `ErrLeaseLost`；
- `TestWriteGuard_NotLeader`：`isLeader=false` → `Authorize()` 返回 `ErrNotLeader`；true → nil；
- `TestWriteGuard_RenewLoop_Failover`：模拟 elector（内存 fake）：主 A 故障 → B RenewLoop 补位 → 写面切到 B（fake elector + 短退避注入，条件轮询断言最终 isLeader=true）。

### 5.2 装配回归（`pkg/server`）

- `TestWriteHandler_LeaderGate`：装配 WriteGuard（isLeader=true）→ 上传 200；isLeader=false → 503 且**业务逻辑未执行**（用 fake uploadStore 断言未调用——防「门后漏写」）；
- `TestWriteHandler_NoLeaderGate`：未装配 WriteGuard（leader.enabled=false）→ 上传 200（零回归）；
- 全量既有 handler 测试在默认（local+enabled）下原样通过。

### 5.3 变异点

| 变异 | 预期红 |
|---|---|
| 把 CAS/TryAcquire 的原子更新拆成「读→判→写」 | 并发抢占测试红 |
| Renew 失败后不置 isLeader=false（继续写） | TestWriteGuard_NotLeader / Failover 红 |
| 移除 `Retry-After` 头 | 装配回归断言红 |
| 从节点仍执行业务逻辑（门后漏写） | TestWriteHandler_LeaderGate 红 |
| 本地锁用 `LOCK_SH`（共享） | 互斥测试红 |
| Windows 回落恒主时**不记 Warn** | 日志断言红（可观测铁律） |

### 5.4 门禁

- **R23 `flock_gate_test.go`**（仿 R19 模式）：扫描非测试源码，禁止直接 `syscall.Flock` / `golang.org/x/sys/unix.Flock` 调用（豁免 `pkg/state` 自身实现与门禁自身）；引导走 `LeaderElector` 抽象；
- 变异验证：在任意生产文件加 `syscall.Flock` → 红；门禁自检（临时目录固定口径）绿。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **F1（Leader）** | `LeaderElector` 接口 + `LocalLeaderElector`（Unix flock + Windows 回落）+ `ErrLeaseLost` + 单测 + R23 门禁 | 互斥测试绿 + 变异命中 + Windows 回落日志断言 | `pkg/state` F1（StateStore 设计） |
| **F2（Leader）** | `MongoLeaderElector` + TTL index + RenewLoop + `WriteGuard` 装配到写面（§2.5 全部写端点）+ 503 语义 | mongo 集成测试绿（本地跳过）；`TestWriteHandler_LeaderGate` 绿；从节点写 503 | F1（Leader）、F3（StateStore mongo 配置） |
| **F3（Leader）** | 配置 `leader` 段 + `RegisterLeaderElector` 注册表接线 + config.md 文档（R15） | 装配测试绿；文档漂移门禁绿 | F2（Leader） |

依赖图：StateStore-F1 → Leader-F1 → Leader-F2 → Leader-F3；Leader-F2 依赖 StateStore-F3 的 mongo 连接装配（共用 `state_store.mongo.*`）。

## 7. 风险与零回归保证

| 风险 | 缓解 |
|---|---|
| 时钟偏移导致短暂双主 | ttl/3 续租 + ±10s 容忍文档化；双主窗口（≤ ttl）内由 CAS（StateStore）兜底检测冲突；Mongo 后端建议 NTP 对齐 |
| 网络分区「双主」 | Renew 失败即让位（fail-closed 方向）；分区两侧只能一侧持有（mongo 是唯一事实源） |
| Windows local 后端无 flock | 回落恒主 + 启动 Warn（可观测，禁静默）；文档限单节点 |
| 写面门漏掉新端点 | 门禁候选（R24 可选：写路由白名单对照表——维护一份「必须过 WriteGuard 的端点清单」，archcheck 断言清单与 routes.go 一致）；本期以代码评审 + 测试覆盖为准 |
| 503 破坏既有客户端 | 单节点默认恒主（行为不变）；多节点部署才可能 503，属预期新语义 |
| 从节点读到旧快照 | 文档明示（读一致性由后续片承担）；写面唯一是本期目标 |

**零回归保证清单**：
1. `leader` 段缺省 `enabled: true` + `type: local` → 单节点 TryAcquire 恒 true、写面恒放行（与现状逐请求等价）；
2. 未装配 WriteGuard（显式 `enabled: false`）→ 零改动旧行为；
3. 既有写面测试套件在默认装配下原样通过；
4. 从节点只影响**写**（503），读面不受影响；
5. Windows 回落恒主有显式 Warn 日志（可观测），不是静默降级。

## 8. 关联项

- StateStore 的 `leader/global` key 可承载**诊断信息**（当前主节点 id / 上任时间），`/api/hub/nodes` 或新 `/api/leader` 端点只读展示（可选片）；
- 集群方案 A 的后续：从节点写转发、读一致性、RaftStateStore（F5 预留）；
- docs/config.md 的 `state_store.leader` 段文档（R15 门禁要求装配期同步）。
