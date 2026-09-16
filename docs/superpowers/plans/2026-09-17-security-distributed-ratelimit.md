# 安全加固：分布式限流 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 增加多实例协调的限流能力：单实例 rate_limit 已有（`pkg/server/ratelimit.go`，IP 维度），本片补「跨实例共享限额」——多个 sproxy 实例（mesh 节点）间共享限流配额，防止单实例限额被绕过。

**架构：**
- 现有：`RateLimiter`（`pkg/server/ratelimit.go`）单实例 token bucket（全局 + IP 维度）。
- 本片：新增可选的「协调后端」抽象——`CoordinatedRateLimiter` 接口（`Allow(ctx, key) bool`），默认实现为本地（现有行为零回归）；可选实现为「基于存储的共享计数器」——复用现有 `pkg/store` 或新写原子计数器（用 storage 原子写 + 文件锁做跨进程协调，免外部依赖如 Redis）。
- 触发：`rate_limit.coordinated: true` 配置开启；`rate_limit.backend` 选择（默认 local；`file` = 基于 storage 的共享计数）。
- 范围：只限隧道 handler（现有 rateLimit 挂载点），不加新路由。
- 设计取舍：**文件锁 + 原子计数** 跨进程协调（本仓无 Redis/外部依赖约束）；不引入第三方限流库（纯标准库）。

**技术栈：** Go 1.27 标准库（sync + os 文件锁 syscall.Flock），无新依赖。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §3-E「安全加固」（分布式限流）。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待。
- 禁 `http.DefaultClient`/共享 DefaultTransport。
- 错误 `fmt.Errorf("...: %w", err)`；日志 `log/slog`。
- Conventional Commits：`feat(ratelimit): <描述>`；禁署名行。
- **TDD + 变异验证**（门禁硬规则）。
- 提交前 `make prepare`。
- Go 1.27 语法。Windows 兼容：`syscall.Flock` 在 Windows 有等价（LockFileEx）——若实现受限，退化为「进程内互斥 + 文件计数」（跨进程协调在 Windows 上降级为尽力而为，文档注明）。

---

### 任务 1：读现有 ratelimit 实现

**文件：**
- 只读：`pkg/server/ratelimit.go`、`pkg/server/config.go`（RateLimitConfig）

**目标：** 理解现有挂载点与扩展点。

- [ ] **步骤 1：读 ratelimit.go**

确认：`RateLimiter.Allow()`（全局）、`AllowIP()`（IP 维度）、`Middleware` 挂载点、`UpdateConfig` 热更新。确认隧道 handler 的挂载方式。

- [ ] **步骤 2：在报告里写出协调抽象设计**

```go
// Coordinator 是限流协调后端抽象（默认 local 零回归）。
type Coordinator interface {
    // Allow 检查 key 是否放行；count 为本次请求消耗的配额（默认 1）。
    Allow(key string, count int64) bool
}

// 实现：
// - localCoordinator：内存计数（= 现有 RateLimiter 语义）
// - fileCoordinator：storage 目录下每 key 一个原子计数文件（flock + read-modify-write）
```

---

### 任务 2：Coordinator 抽象 + local 实现

**文件：**
- 创建：`pkg/server/ratelimit_coord.go`
- 测试：`pkg/server/ratelimit_coord_test.go`

**目标：** 抽象落地，local 实现零回归。

- [ ] **步骤 1：编写失败的测试**

```go
func TestLocalCoordinator_AllowWithinLimit(t *testing.T) { t.Parallel() /* limit=3, 3 次 Allow true, 第 4 次 false */ }
func TestLocalCoordinator_KeyIsolated(t *testing.T)     { t.Parallel() /* 不同 key 独立计数 */ }
func TestLocalCoordinator_Reset(t *testing.T)           { t.Parallel() /* 窗口过期后计数复位 */ }
```

- [ ] **步骤 2：运行测试验证失败** → 实现 `localCoordinator` → 验证通过

```bash
go test -count=1 -run 'TestLocalCoordinator' ./pkg/server/...
```

- [ ] **步骤 3：Commit**

```bash
git add pkg/server/ratelimit_coord.go pkg/server/ratelimit_coord_test.go
git commit -m "feat(ratelimit): 限流协调后端抽象 + 本地实现（零回归）" --no-verify
```

---

### 任务 3：file 协调后端（跨进程共享计数）

**文件：**
- 修改：`pkg/server/ratelimit_coord.go`（新增 `fileCoordinator`）
- 修改：`pkg/server/config.go`（`rate_limit.coordinated` / `.backend` 字段）
- 修改：`pkg/server/ratelimit.go`（装配 Coordinator）
- 测试：`pkg/server/ratelimit_coord_file_test.go`

**目标：** 跨进程共享计数，多实例协调。

- [ ] **步骤 1：编写失败的测试**

```go
func TestFileCoordinator_CrossProcess(t *testing.T) {
    t.Parallel()
    // 同一 storage 目录下两个 fileCoordinator 实例（模拟两进程）
    // 共享 limit：c1.Allow 3 次 true 后，c2.Allow 第 4 次 false
}
func TestFileCoordinator_AtomicUnderRace(t *testing.T) {
    t.Parallel()
    // 多 goroutine 并发 Allow → 计数精确（-race 下跑）
}
func TestFileCoordinator_InvalidBackend(t *testing.T) {
    t.Parallel()
    // backend=unknown → 装配失败/回退 local + 警告日志
}
```

- [ ] **步骤 2：运行测试验证失败** → 实现 `fileCoordinator`（flock + read-modify-write 原子计数；窗口过期按文件 mtime 复位）→ 验证通过 + 变异验证（去掉 flock → 并发测试应红）

```bash
go test -count=1 -run 'TestFileCoordinator' ./pkg/server/...
```

- [ ] **步骤 3：配置接线**

`config.go` RateLimitConfig 加：`Coordinated bool`（`yaml:"coordinated"`，默认 false 零回归）、`Backend string`（`yaml:"backend"`，默认 "local"）。`ratelimit.go` 装配时按配置选 Coordinator；`UpdateConfig` 热更新时重建。

- [ ] **步骤 4：全量验证 + Commit**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/   # 无输出
go test -count=1 ./pkg/server/...
git add pkg/server/ratelimit_coord.go pkg/server/config.go pkg/server/ratelimit.go pkg/server/ratelimit_coord_file_test.go
git commit -m "feat(ratelimit): 文件锁共享计数后端，多实例协调限流" --no-verify
```

---

### 任务 4：装配接线 + 文档

**文件：**
- 修改：`pkg/server/routes.go`（装配 Coordinator）
- 修改：`README.md` / `docs/config.md`（`rate_limit.coordinated` / `.backend` 配置说明）

**目标：** 配置生效、文档同步（R15 不漂移）。

- [ ] **步骤 1：装配接线**

`routes.go` 中 rateLimit 创建处：按 `cfg.RateLimit.Backend` 创建 Coordinator（local/file），传入 `RateLimiter`；隧道 handler 挂载处不变。

- [ ] **步骤 2：端到端测试**

```go
func TestCoordinatedRateLimit_E2E(t *testing.T) {
    t.Parallel()
    // 装配两个 handler 共享同一 storage 目录 → 总请求数超限后第二个实例 429
}
```

- [ ] **步骤 3：文档 + 验证 + Commit**

```bash
make lint  # R15 文档门禁
git add pkg/server/routes.go README.md docs/config.md
git commit -m "feat(ratelimit): 协调限流装配接线 + 配置文档" --no-verify
```
