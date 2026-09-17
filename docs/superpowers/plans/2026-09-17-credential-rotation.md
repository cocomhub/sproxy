# 凭据定期自动轮换 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 凭据增加**定期自动轮换调度器**：按配置周期检查各 AK 的最旧 SK 到期时间，到期前自动 renew（复用现有 `/api/credentials/{ak}/renew` 逻辑），并按 keep_old 裁剪过期旧 SK。方案已由用户 2026-09-17 确认。

**架构：**
- 配置新增（`pkg/server/config.go` `CredentialsConfig`）：
  - `rotation.interval`（duration，默认 0 = 关闭，零回归）
  - `rotation.notify_before`（duration，默认 168h = 7 天：到期前 7 天开始轮换）
  - `rotation.keep_old`（int，默认 2：保留旧 SK 数，>1 时新 SK 生效后旧 SK 宽限期可用）
- 调度器 goroutine（仿 `versionGCLoop` 模式）：ticker 按 interval 触发 → 遍历 Ring 各 AK → 检查最旧 SK `expires_at ≤ now+notify_before` → 命中调 `renewCredential`（复用现有逻辑）→ 审计 → 清理超过 keep_old 的旧 SK（复用 skExpire 逻辑）。
- 幂等：同一 AK 一轮只轮换一次；调度器退出走 Close() 的 stop channel（仿 versionGCStop）。
- 测试：时钟注入（synctest 或可注入 now 函数）+ 断言「到期前被轮换 / 未到期不轮换 / keep_old 裁剪 / interval=0 不启动」。

**技术栈：** Go 1.27 标准库。

**规格：** 用户 2026-09-17 决策：先做定期自动轮换（服务端调度器）；跨节点凭据同步暂缓。

## 全局约束

- UTF-8 without BOM；SPDX 头；测试纯标准库；只绑 127.0.0.1；顶层 `TestX` 默认 `t.Parallel()`（R18）。
- 禁 `time.Sleep`（R14）；synctest/事件等待（可注入 now 函数）。
- 日志 `log/slog`；错误 `fmt.Errorf("...: %w", err)`。
- Conventional Commits：`feat(credentials): <描述>`；禁署名行。
- 提交前 `make prepare`。
- Go 1.27 语法。

---

### 任务 1：配置字段 + 调度器核心

**文件：**
- 修改：`pkg/server/config.go`（CredentialsConfig 加 rotation 三字段）
- 修改：`pkg/server/config_defaults.go`（默认值）
- 创建：`pkg/server/credential_rotation.go`（调度器 + Pass 逻辑）
- 测试：`pkg/server/credential_rotation_test.go`

**目标：** 调度器核心可测。

- [ ] **步骤 1：读现有凭据结构**

读 `pkg/server/credentials_handler.go`（`renewCredential`/`skExpireHandler`）、`pkg/accesskey`（Ring/SK 结构：`ExpiresAt` 字段名以代码为准）、`pkg/server/config.go` 的 `CredentialsConfig`。

- [ ] **步骤 2：编写失败的测试**

```go
// pkg/server/credential_rotation_test.go
func TestCredentialRotation_NotDue_NoAction(t *testing.T) {
    t.Parallel()
    // Ring 装配：AK 的 SK expires_at 在 now+30d → 一轮 Pass 不轮换（无新增 SK）
}
func TestCredentialRotation_Due_Renews(t *testing.T) {
    t.Parallel()
    // SK expires_at 在 now+1d（< notify_before 7d）→ 一轮 Pass 调 renew → 新增 SK 条目
}
func TestCredentialRotation_KeepOld_Prunes(t *testing.T) {
    t.Parallel()
    // keep_old=2，已有 3 个 SK → Pass 后最旧 1 个被裁剪（expire）
}
func TestCredentialRotation_Disabled_NoLoop(t *testing.T) {
    t.Parallel()
    // interval=0 → 不启动 goroutine（无副作用）
}
```

> 需要可注入的时钟：`rotationPass(now func() time.Time)` 或结构体字段 `now func() time.Time`（默认 time.Now）。测试注入假时钟。

- [ ] **步骤 3：运行测试验证失败**

运行：`go test -count=1 -run TestCredentialRotation ./pkg/server/`
预期：FAIL

- [ ] **步骤 4：实现**

`credential_rotation.go`：
```go
// rotationConfig 是凭据轮换调度配置（从 cfg 拷贝，避免热更新竞态）。
type rotationConfig struct {
    interval     time.Duration
    notifyBefore time.Duration
    keepOld      int
}

// credentialRotationLoop 按 interval 周期执行一轮凭据轮换检查。
// 作为 goroutine 在 RegisterRoutes 中按配置启动；由 Close() 关闭 stop channel 停止。
// 与 versionGCLoop 同构（ticker + stop + WaitGroup）。

// rotationPass 执行一轮检查：遍历 Ring 所有 AK，最旧 SK 到期前 notifyBefore 内 → renew；
// 超过 keep_old 的旧 SK → expire。幂等：一轮内同一 AK 只轮换一次。
```

- [ ] **步骤 5：运行测试验证通过 + 变异验证（去掉到期判断 → Due 测试红）**

运行：`go test -count=1 -run TestCredentialRotation ./pkg/server/`
预期：PASS

- [ ] **步骤 6：全量验证 + Commit**

```bash
go build ./...
gofmt -l pkg/server/ goimports -l pkg/server/
go test -count=1 ./pkg/server/...
git add pkg/server/config.go pkg/server/config_defaults.go pkg/server/credential_rotation.go pkg/server/credential_rotation_test.go
git commit -m "feat(credentials): 凭据定期自动轮换调度器（到期前通知+keep_old 裁剪）" --no-verify
```

---

### 任务 2：装配接线 + 热更新 + 文档

**文件：**
- 修改：`pkg/server/routes.go`（启动/停止 rotation loop）
- 修改：`pkg/server/handlers.go`（Handlers 结构加 rotationStop/rotationWg）
- 修改：`pkg/server/handlers_lifecycle.go`（Close 关闭 rotationStop）
- 测试：`pkg/server/credential_rotation_test.go`（补 loop 启动/停止测试）
- 修改：`README.md` / `docs/config.md`（credentials.rotation 配置说明）

**目标：** 调度器装配进 RegisterRoutes，文档同步。

- [ ] **步骤 1：装配**

`routes.go` 中（仿 versionGCLoop 启动点）：`if cfg.Credentials.Rotation.Interval > 0 { h.rotationStop = make(chan struct{}); h.rotationWg.Add(1); go h.credentialRotationLoop() }`；`Close()` 关 rotationStop + Wait。

- [ ] **步骤 2：热更新**

`UpdateConfig`（PUT /api/config）时若 rotation 配置变化 → 停旧 loop 启新 loop（或文档注明需重启，仿现有硬配置语义）。**简化：与 SIGHUP 硬配置一致，rotation 变更需重启，文档注明。**

- [ ] **步骤 3：测试 loop 启动/停止**

```go
func TestCredentialRotation_LoopStartStop(t *testing.T) {
    t.Parallel()
    // interval>0 装配 → loop 运行；Close() → 退出（无 goroutine 泄漏）
}
```

- [ ] **步骤 4：文档 + 全量验证 + Commit**

```bash
make lint
go test -count=1 ./pkg/server/...
git add pkg/server/routes.go pkg/server/handlers.go pkg/server/handlers_lifecycle.go pkg/server/credential_rotation_test.go README.md docs/config.md
git commit -m "feat(credentials): 轮换调度器装配接线 + 配置文档（interval/notify_before/keep_old）" --no-verify
```
