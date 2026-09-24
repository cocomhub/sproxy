# 通用任务调度器设计（S3-H2，roadmap 11.10-H2）

> 状态：DRAFT（供 design-batch 审校）。只读源码：pkg/server/version.go、handlers_endpoints.go、share.go、handlers.go、config.go。

## 1. 背景与目标

**现状**：version / trash / share / upload 共 4 个周期 GC 循环各自 `ticker + stopCh + WaitGroup` 分散实现（另 mirror / tier / indexSave / rotation 4 个循环同构，属后续可迁移对象）：

| 循环 | 位置 | 间隔 | 停止信号 | 备注 |
|------|------|------|----------|------|
| `cleanupUploadingFilesLoop` | handlers_endpoints.go | 固定 10min | `uploadingStop/uploadingWg` | 委托 `cleanupUploadingFilesPass` |
| `versionGCLoop` | handlers_endpoints.go | `cfg.Versioning.GCInterval` | `versionGCStop/versionGCWg` | 仅 `gc_interval>0` 时挂载 |
| `trashGCLoop` | handlers_endpoints.go | `cfg.Trash.GCInterval`（默认 1h） | `trashGCStop/trashGCWg` | TTL 每 tick 现读（热更新可见） |
| `ShareStore.cleanupLoop` | share.go | 固定 5min | `stopCh/stopOnce/wg` | 自带 panic 恢复 + 退出前补清一次 |

**目标**：新增统一调度器 `pkg/server/scheduler.go`（注册周期任务 + 维护窗口 + panic 恢复 + 单飞防重入），把上述 4 循环迁移到调度器；默认间隔/行为不变，零回归。

## 2. 组件与接口

### 2.1 `pkg/server/scheduler.go`（新文件，同包 server）

```go
// TaskFunc 任务执行体（ctx 供未来超时/取消扩展；本期恒 Background）。
type TaskFunc func(ctx context.Context)

type Task struct {
    Name            string        // 唯一；重名 Register 报错
    Interval        time.Duration // >0 才允许注册（防 0 间隔忙循环）
    Run             TaskFunc
    MaintenanceOnly bool          // true = 维护窗口外跳过（未配窗口 = 恒执行）
    FinalRunOnStop  bool          // true = Stop 时补跑一次（share「退出前清理」语义）
}

type Scheduler struct { /* mu / tasks / logger / stopCh / wg / inWindow func(time.Time) bool */ }

func NewScheduler(logger *slog.Logger) *Scheduler
func (s *Scheduler) Register(t Task) error            // 校验 Name 非空、Interval>0、重名
func (s *Scheduler) SetMaintenanceWindow(in func(time.Time) bool) // nil = 关闭窗口（默认，零回归）
func (s *Scheduler) Start()                            // 幂等（sync.Once）：为每个任务起 goroutine
func (s *Scheduler) Stop()                             // 幂等（closeOnce）：关 stopCh → wg.Wait；FinalRunOnStop 任务补跑
```

- **单飞防重入**：`taskEntry.running atomic.Bool`，`CompareAndSwap(false,true)` 失败 → Warn「上次未结束，跳过本 tick」；`defer` 复位。
- **panic 恢复**：`runOnce` 内 `defer recover()` → `logger.Error("scheduler task panic", "task", name, "panic", r)`，循环继续（下个 tick 照常）。
- **goroutine 形状**（与现循环同构）：`ticker.Stop()` defer + `select { <-stopCh / <-t.C }`；窗口判断在 `t.C` 分支内 `continue`（下 tick 再查）。

### 2.2 配置（config.go 新增）

```go
type MaintenanceWindowConfig struct {
    Enabled bool   `yaml:"enabled"` // 默认 false = 零回归
    Start   string `yaml:"start"`   // "HH:MM" 24h；End<=Start 视为跨午夜
    End     string `yaml:"end"`
}
type SchedulerConfig struct { MaintenanceWindow MaintenanceWindowConfig `yaml:"maintenance_window"` }
// Config 新增字段：
Scheduler SchedulerConfig `yaml:"scheduler"`
```

装配层把窗口解析为 `inWindow func(time.Time) bool`；非法 HH:MM → `config_validate.go` Validate 拒绝（fail-closed，对齐 VolumeMeshReaderConfig scope 先例）。

## 3. 数据流

1. **装配（RegisterRoutes）**：`NewScheduler(logger)` → 按配置 Register：
   - `uploading-cleanup`：恒注册，10min，Run = `cleanupUploadingFilesPass`（MaintenanceOnly=false，防泄漏优先级高）；
   - `version-gc`：仅 `cfg.Versioning.GCInterval>0` 注册，Run = `gcAllExpiredVersionsPass`（MaintenanceOnly=true）；
   - `trash-gc`：注册，间隔 = `cfg.Trash.GCInterval`（≤0 用默认 1h，保留现逻辑），Run = 每 tick 现读 TTL 遍历租户 `CleanupTrash`（MaintenanceOnly=true）；
   - `share-cleanup`：5min，Run = `shareStore.cleanupExpired()`（MaintenanceOnly=false），`FinalRunOnStop=true`。
2. **运行**：每 tick →（仅 MaintenanceOnly 任务）查窗口，窗口外 continue → 单飞 CAS → defer recover 包裹 Run。
3. **停止（Close 内）**：`h.scheduler.Stop()` 取代逐个 `close(versionGCStop/trashGCStop/uploadingStop)`——关 stopCh 后各 goroutine 退出（share 补跑一次）→ `wg.Wait()` → 继续既有 Close 步骤（UploadStore/audit/shareStore.Stop 等，顺序不变）。

## 4. 错误处理

- **任务 panic**：recover + Error 日志（含任务名），循环存活；share 原有 recover 迁入调度器后删除其自身 recover（幂等保留亦可，二选一）。
- **重名 / Interval<=0 / Name 空**：`Register` 返回 error，装配层 Error 日志后跳过该任务（不 panic）。
- **单飞跳过**：Warn 日志；如长期任务+短间隔产生噪音，后续加 `sprox_scheduler_skips_total` metric（独立片）。
- **窗口解析失败**：Validate 期拒绝启动（配置错误响亮暴露）。
- **Stop 无超时**：任务挂死阻塞停服——与现状逐循环 `wg.Wait()` 语义一致，非回归（文档明示）。

## 5. 测试与变异点

1. `TestScheduler_RegisterStartStop`：20ms 间隔 + 条件等待计数≥2 → Stop → 计数冻结。变异：Start 不启 goroutine → 红。
2. `TestScheduler_PanicRecovery`：Run 首次 panic → 下一 tick 仍执行（计数达 2）。变异：去掉 recover → 测试进程 panic 红。
3. `TestScheduler_NoReentrant`：Run 睡 100ms、间隔 20ms → 并发峰值计数恒 1（原子 max）。变异：去掉 CAS 单飞 → 并发>1（-race 红）。
4. `TestScheduler_MaintenanceWindow`：窗口外 MaintenanceOnly 任务 0 次、非 MaintenanceOnly 照跑；窗口内恢复。变异：窗口判断反向 → 红。
5. `TestScheduler_FinalRunOnStop`：FinalRunOnStop 任务 Stop 后再 +1。变异：去掉补跑 → 红。
6. `TestScheduler_RegisterValidation`：重名/零间隔/空名 → error。变异：允许重名 → 双 goroutine 同任务红。
7. `TestScheduler_MigratedLoops_ZeroRegression`（装配级）：新装配下 version 间隔取 cfg、trash TTL 改小后下轮清理生效（TTL 现读保留）、share 5min + 退出前补清一次、upload 10min。

## 6. 片划分

- **片1（S3-H2-a）**：scheduler.go 核心（Task/Register/Start/Stop/单飞/recover/窗口）+ 单测 1–6。
- **片2（S3-H2-b）**：Handlers 三循环迁移（upload/version/trash）：删 `uploadingStop/uploadingWg/versionGCStop/versionGCWg/trashGCStop/trashGCWg` 字段与对应 Close 分支 → `h.scheduler` 字段；装配级回归测试 7。
- **片3（S3-H2-c）**：ShareStore 迁移 + 维护窗口配置（config 结构 + Validate + SetMaintenanceWindow 接线）+ 文档（docs/config.md scheduler 段、docs/architecture.md 调度器节）。
- **片4（后续片，不在本期）**：mirror/tier/indexSave/rotation 同构迁移（复用同一 Scheduler，避免本期膨胀）。

## 7. 风险与零回归保证

- **零回归**：间隔默认值 1:1（10m / 5m / 1h / gc_interval）；各 pass 函数零改动；退出语义一致（share 补跑保留；其余循环退出即返回）；维护窗口默认关闭（inWindow=nil 恒执行）；panic 恢复为净新增保护（无退化）。
- **ShareStore 迁移风险**：`NewShareStore` 移除自动 goroutine 后，直接构造 ShareStore 的测试不再自动清理 → 包内测试改显式调 `CleanupExpired()`（导出薄封装，锁内委托 cleanupExpired），一次性更新并标注。生产装配（RegisterRoutes）节奏 1:1 保留。
- **旧字段删除风险**：`versionGCStop` 等字段被手工构造 Handlers 的测试引用 → 片2 先 grep 全量更新引用再删字段。
- **Stop 挂死**：与现状一致（无超时），文档明示；如需超时属独立改造。
- **窗口跨午夜**：`End<=Start` 视为跨天窗口，验证用例覆盖。
