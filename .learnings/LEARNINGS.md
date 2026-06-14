# Learnings

Corrections, insights, and knowledge gaps captured during development.

**Categories**: correction | insight | knowledge_gap | best_practice

---

## [LRN-20260614-BP1] viper.New ReadInConfig

**Logged**: 2026-06-14T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: config

### Summary
`viper.New()` 创建独立实例后必须调用 `ReadInConfig()`，否则配置文件永远不会被读取。

### Details
将 `viper.GetViper()` 迁移到 `viper.New()` 时很容易忘记 `ReadInConfig()`。`viper.New()` 创建干净的 viper 实例，不会自动加载配置——必须显式调用 `ReadInConfig()`，且要正确处理 `ConfigFileNotFoundError`（文件不存在时放行，允许纯 flag/env 模式）。

### Suggested Action
已在 tunnel.go、root.go 修复。已推广到 CLAUDE.md 和 memory。

### Metadata
- Source: code_review
- Pattern-Key: config.viper.read_config
- Recurrence-Count: 1
- First-Seen: 2026-06-14

---

## [LRN-20260614-BP2] cobra Run -> RunE migration

**Logged**: 2026-06-14T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: tests

### Summary
Cobra 命令从 `Run` + `os.Exit(1)` 迁移到 `RunE` + `return error`，使测试不再被进程杀死。

### Details
- `RunE` 返回 error 后，cobra 的 `rootCmd.Execute()` 不主动 `os.Exit`，error 返回调用者
- 生产入口 `main()` 中 `Execute()` 负责 `os.Exit(1)`
- 测试直接调用 `rootCmd.Execute()` 可捕获 error，不再 `t.Skip`
- `PersistentPreRunE` 在 `RunE` 之前执行，共享命令上下文

### Suggested Action
新命令一律用 `RunE`。已推广到 memory。

### Metadata
- Source: refactoring
- Pattern-Key: cobra.command.rune
- Recurrence-Count: 1
- First-Seen: 2026-06-14

---

## [LRN-20260614-BP3] signal goroutine leak fix pattern

**Logged**: 2026-06-14T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: backend

### Summary
`for sig := range signalChan` 在 `ListenAndServe` 失败时泄漏 goroutine。修复方案：`stopSigCh` + `select` 模式或 `signal.NotifyContext`。

### Details
`signal.Notify` 创建的 channel 永远不会被运行时关闭，如果 `ListenAndServe` 返回 error 后 goroutine 还阻塞在 `for range signalChan` 上就泄漏。使用 `stopSigCh := make(chan struct{})` + `close(stopSigCh)` + `select` 或 `signal.NotifyContext` 解决。

### Metadata
- Source: refactoring
- Pattern-Key: backend.signal.leak
- Recurrence-Count: 1
- First-Seen: 2026-06-14

---

## [LRN-20260614-BP4] error must be returned, never swallowed

**Logged**: 2026-06-14T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: backend

### Summary
所有可能失败的操作必须返回 `(T, error)`，禁止静默吞错误。

### Details
`xdgDirPersister.LoadDir` 和 `SaveDir` 原始设计返回空值和 void，静默吞错误。改为返回 `(string, error)` 和 `error`。`os.IsNotExist` 时返回空值不是错误，其他错误向上传播。

### Metadata
- Source: code_review
- Pattern-Key: error_handling.no_swallow
- Recurrence-Count: 1
- First-Seen: 2026-06-14

---

## [LRN-20260614-BP5] go work multi-module cmd isolation

**Logged**: 2026-06-14T09:00:00Z
**Priority**: medium
**Status**: promoted
**Area**: config

### Summary
monorepo 中 cmd 目录独立为 go module 时，必须用 `go.work` manage，cmd go.mod 用 `replace` 指向根 module。

### Details
- `go.work` 的 `use` 指令管理所有 module
- cmd 的 `go.mod` 用 `replace github.com/cocomhub/sproxy => ../../` 引用根 module
- `go build ./cmd/sproxy/...` 在 go.work 下通过 `go.work` 查找
- `make test` 需要分别测试 `./cmd/sproxy/...` 和 `./cmd/sclient/...`（不再归入 `./cmd/...`）
- `pkg/testutil` 通过根 module path 导入，cmd module 可直接使用

### Metadata
- Source: refactoring
- Pattern-Key: config.go_module.cmd
- Recurrence-Count: 1
- First-Seen: 2026-06-14
