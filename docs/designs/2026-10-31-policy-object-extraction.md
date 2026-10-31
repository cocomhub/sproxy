# 装配层策略对象抽取（P1）

> 状态：设计（2026-10-31）。前置：server 治理 S1–S13（13 个 `pkg/server/<cap>` 叶子包已下沉，
> `pkg/server` 42,568 → 32,849 行）。本文解决**剩余装配核心**的可修改性，不再按文件搬包。

## 1. 为什么不再切包

实测判据（写入 `docs/architecture.md`）：`cloud`/`volume`/`files` 的装配依赖 30+ 个，而叶子包
0–8 个；抽出后必须在 `Deps` 里复刻 `Handlers` 能力面，**耦合不降反升**。正确做法是把散落的
**决策逻辑**收敛成有行为、有不变量、可单测的策略对象，装配层只做「取参数 → 调策略 → 写响应」。

## 2. 三个目标对象

### 2.1 `volume.Router`（卷路由与 ACL）

**问题**：`locateForRead` / `locateOwnerFile` / `locateOwnerFileLegacy` / `locateOwnerFileScan` /
`volumeAllowedFor` / `defaultVolumeAllows` 分散在 `volumes.go`（约 1.3k 行）与 `volumes_api.go`，
同一语义有多份实现，ACL 判定与定位逻辑交织。

**目标**：`pkg/volume/router.go`（Global 能力包，server 与 sclient 均可复用）

```go
type Router struct{ set *Set }              // 只读视图 + 卷状态
func (r *Router) Locate(owner, rel string) (Location, bool)   // 唯一实现：默认卷快路径 → 遍历视图
func (r *Router) Allowed(owner, volName string) bool          // ACL 单点
type Location struct{ Volume string; Tenant *storage.Tenant }
```

**不变量**：
- 同一 `rel` 最多命中一个卷（AD-4 唯一性）；多卷命中时默认卷优先（防御性）。
- `Locate` 不创建租户目录（只读、无副作用）。
- `Allowed` 对未装配卷集 / 非法 owner 一律 fail-closed。

**验收**：`pkg/volume/router_test.go` 覆盖多卷同名、默认卷 ACL 排除、未装配、非法 owner；
`pkg/server` 中 4 处定位实现收敛为 1 处（行数下降 ≥30%）。

### 2.2 `quota.Settlement`（配额结算协议）

**问题**：`Reserve → 写 → Reconcile → Commit/Release` 的调用顺序散落在 cloud 归档、卷迁移、
版本保存、S3 complete 等处；顺序错/漏回滚会导致**配额泄漏或超卖**。

**目标**：`pkg/quota/settlement.go`

```go
type Settlement struct{ /* 预留句柄 + 已提交量 + 状态 */ }
func (s *Settlement) Reserve(n int64) error
func (s *Settlement) Reconcile(actual int64)      // 按落盘真值调整预留
func (s *Settlement) Commit() error               // 幂等
func (s *Settlement) Release()                    // 幂等；未 Commit 则全额回滚
```

**不变量**：
- `Commit`/`Release` 幂等；`Release` 后不得再 `Commit`。
- 未 `Commit` 的 `Settlement` 在 `Close`/GC 时自动 `Release`（防泄漏）。
- 状态机非法转换返回哨兵错误（不 panic）。

**验收**：`pkg/quota/settlement_test.go` 覆盖并发预留、回滚、重复 Commit/Release、异常路径；
至少 3 个调用点（cloud 归档 / 卷迁移 / 版本保存）改为使用同一实现。

### 2.3 `cloudtasks.Orchestrator`（云任务编排）

**问题**：`cloud_download_handler.go`(1357) + `cloud_archive_handler.go`(844) 依赖约 30 个
`Handlers` 能力（配额、卷路由、审计、写锁、staging gate…）。

**目标**：`pkg/server/cloudtasks`，依赖收敛为 6–8 个**窄接口**：

```go
type Deps struct {
    Tasks   TaskStore        // 任务 CRUD
    Sources SourceFS         // 源读取（cloud/ 桶）
    Targets TargetWriter     // 目标写入（user/ 桶，含配额）
    Quota   QuotaSettler     // 见 2.2
    Router  VolumeRouter     // 见 2.1
    Audit   AuditSink
    Logger  *slog.Logger
}
```

**验收**：`Deps` 字段 ≤ 8（R52 门禁守）；HTTP 面留在装配层；单测覆盖「单任务 / 批量 / 组 /
归档到卷」四条主路径与失败回滚。

## 3. 实施顺序与门禁

| 步骤 | 内容 | 门禁 |
|---|---|---|
| P1-a | `volume.Router` 抽取 + 4 处调用收敛 | R6/R7；新增 `Router` 单测；`pkg/server` 行数 −30% |
| P1-b | `quota.Settlement` 抽取 + 3 调用点迁移 | 不变量测试 + 并发测试 |
| P1-c | `cloudtasks.Orchestrator`（Deps ≤8） | R52 字段上限；四条主路径 e2e |

**禁止**：为过门禁放宽 R52 上限、把策略逻辑留在装配层同时新增薄封装。

## 4. 与已有纪律的关系

- 依赖注入只走构造期（AGENTS 7b），不替换包级变量。
- 单测纯标准库、`t.Parallel()`、只绑 127.0.0.1。
- 新增能力包必须登记 `internal/archcheck/layers.go`（Levels/Managed/ParentDomain）。
