# sproxy 全面优化设计文档

## 概述

基于当前 sproxy 项目的测试覆盖完善阶段，对项目进行系统性优化。包括：cmd 独立 Go module 实现依赖隔离、修复已知技术债务、CLAUDE.md 按需加载优化、以及整体回归验证。

## 背景

经过多轮测试覆盖完善（+4.2% 覆盖率 → 70.2%），项目已进入稳健阶段。CLAUDE.md 记录了 5 项已知技术债务，同时存在 cmd 依赖耦合、viper 全局单例使用、CLAUDE.md skills 列表冗余等问题。

## 方案选择

### cmd 独立 module

```
go.mod (github.com/cocomhub/sproxy)
├── pkg/server/    — 根 module
├── pkg/client/    — 根 module（保留 adrg/xdg 依赖）
├── pkg/tunnel/    — 根 module
├── pkg/testutil/  — 根 module
go.work
├── .              — 根 module
├── cmd/sproxy/    — [新] 独立 go.mod, replace → ../..
├── cmd/sclient/   — [新] 独立 go.mod, replace → ../..
├── pkg/tunnel/xfer/ext/ws/
├── pkg/tunnel/xfer/ext/quic/
```

cmd 独立 module 的依赖链：

| cmd 独立 module | 需引用根 module 包 | 自身 go.mod 直接依赖 |
|---|---|---|
| `cmd/sproxy` | pkg/server, pkg/tunnel, pkg/testutil | cobra, viper |
| `cmd/sclient` | pkg/client, pkg/testutil | cobra, viper |

`pkg/server` 内部的 `gopkg.in/yaml.v3` 通过 Go module 最小版本选择自动传递，cmd module 无需显式 require。

### 全局状态结构体化（消除全局变量 + 移除 xdg 从主包）

**原则：除 cobra 命令入口（Run/RunE）外，所有函数避免依赖全局变量。全局状态只在命令入口处读取，业务逻辑通过结构体方法或参数传入。**

**cmd/sclient 重构方案：**

```go
// 1. 持久化接口（解耦 xdg）
type dirPersister interface {
    LoadDir() string
    SaveDir(dir string)
}

// 2. XDG 持久化实现（唯一保留 xdg import 的地方）
type xdgDirPersister struct{}
func (p *xdgDirPersister) LoadDir() string { ... }  // 调用 xdg.CacheFile
func (p *xdgDirPersister) SaveDir(dir string) { ... }

// 3. 空持久化（测试用，不读写文件）
type noopDirPersister struct{}
func (p *noopDirPersister) LoadDir() string  { return "" }
func (p *noopDirPersister) SaveDir(_ string) {}

// 4. 应用状态结构体 — 所有包级变量归入此处
type appState struct {
    cfgFile    string
    currentDir string
    persister  dirPersister
}

// 方法封装业务逻辑（不依赖全局变量）
func (s *appState) ResolveRemotePath(userPath string) (string, error) { ... }
func (s *appState) SaveCurrentDir() { ... }
func (s *appState) LoadCurrentDir() { ... }

// 5. 全局实例（仅 cobra Run/RunE 入口引用）
var globalState = &appState{
    persister: &xdgDirPersister{},
}
```

**函数迁移对照：**

| 当前函数 | 全局变量依赖 | 改为 |
|---|---|---|
| `resolveRemotePath(userPath)` | `currentDir` | `appState.ResolveRemotePath(userPath)` |
| `mustResolveRemotePath(userPath)` | `currentDir` + `os.Exit` | 调用 `s.ResolveRemotePath()` + 在 `RunE` 中 `return err` |
| `saveCurrentDir()` | `currentDir` | `appState.SaveCurrentDir()` |
| `loadCurrentDir()` | `currentDir` | `appState.LoadCurrentDir()` |
| `buildFileClient(cmd)` | `viper.GetViper()` | 接收 viper 实例：`buildFileClient(cmd, v *viper.Viper)` |
| `initLogger(verbose)` | 无（纯函数） | 保持不动 |

**cobra Run → RunE 入口模式：**

```go
var cdCmd = &cobra.Command{
    Use: "cd [path]",
    RunE: func(cmd *cobra.Command, args []string) error {
        // 只有这里访问 globalState
        if err := globalState.execCd(args); err != nil {
            return err
        }
        return nil
    },
}

// 业务逻辑：纯方法，不依赖全局变量
func (s *appState) execCd(args []string) error {
    // 使用 s.currentDir、s.persister
    ...
}
```

**测试并发支持：**

```go
func TestResolveRemotePath_Parallel(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name       string
        currentDir string
        input      string
        want       string
        wantErr    bool
    }{...}
    for _, tc := range tests {
        tc := tc
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            // 每个子测试创建独立的 appState，完全不共享全局变量
            s := &appState{currentDir: tc.currentDir}
            got, err := s.ResolveRemotePath(tc.input)
            ...
        })
    }
}
```

**对于 `archive.go` 中的 `var _ = client.FileInfo{}` 等导入断言：** 改为在测试文件中声明，或使用 `import _ "..."`。同时发现 `archive.go:98-102` 存在多余的 `var _ = ...` 断言（用于确保接口编译期检查），属于历史遗留，将迁移到测试文件。

**cmd/sproxy 状态结构体设计：**

```go
type sproxyState struct {
    cfgFile              string
    cfgPtr               atomic.Pointer[server.Config]
    currentTunnelKeyHex  string
    testSignalCh         chan os.Signal  // 仅测试注入
}

var globalState = &sproxyState{}
```

sproxy 的全局变量本来就少，且大部分只在 `runServer` 和 `handleSighup` 中使用。关键变化：
- `currentTunnelKeyHex` 改为 `sproxyState` 字段
- `handleSighup` 方法化：`func (s *sproxyState) handleSighup(oldCfg *server.Config, tunUpdater server.TunnelUpdater)`
- `resolveTunnelKey` 接收 `cfgFile` 参数而非引用全局变量


### 技术债务修复清单

| # | 问题 | 解决方式 | 涉及文件 | 优先级 |
|---|---|---|---|---|
| 1 | `captureStdout` 重复 | cmd 独立后导入 `pkg/testutil`，删除重复代码 | `cmd/sproxy/root_test.go`, `cmd/sclient/cmd_test.go` | 高 |
| 2 | 信号 goroutine 泄漏 | signal channel 在 `ListenAndServe` 失败时关闭 | `cmd/sproxy/root.go` | 高 |
| 3 | `os.Exit(1)` 导致测试需 Skip | 所有 `Run` 命令改为 `RunE`，`mustResolveRemotePath` → 返回 error | `cmd/sclient/cd.go`, `cmd/sclient/config.go`, 全部 cmd 文件 | 高 |
| 4 | `findModuleRoot` 冗余 | 删除 `test/e2e_test.go` 中的文件遍历，统一用 `runtime.Caller` | `test/e2e_test.go` | 低 |
| 5 | `newTestServerWithAllRoutes` 路由重复 | 添加自动注册验证测试（沿用已有 `server_handler_gaps_test.go` 模式） | `pkg/server/integration_test.go` | 低 |
| 6 | `viper.GetViper()` 全局单例 | 生产代码改用 `viper.New()` 创建独立实例 | `cmd/sproxy/root.go`, `cmd/sclient/root.go` | 中 |

### 全局变量消除清单

| module | 全局变量 | 消除方案 |
|---|---|---|
| cmd/sproxy | `cfgFile string` | 保持（`init()` 中由 `StringVar` 绑定，cobra 需要） |
| cmd/sproxy | `cfgPtr atomic.Pointer[server.Config]` | 保持（`RegisterRoutes` 需要指针更新） |
| cmd/sproxy | `currentTunnelKeyHex string` | 改为 `runServer` 局部变量 |
| cmd/sproxy | `testSignalCh chan os.Signal` | 保持（测试注入所需） |
| cmd/sclient | `cfgFile string` | 保持 |
| cmd/sclient | `currentDir string` | **消除**：包装为 `currentDirState` 结构体，或通过 XDG 持久化接口封装，测试时注入 mock 避免共享状态 |

### Cobra 命令全部改用 RunE

当前使用 `Run`（非 RunE）的命令清单，全部改为 `RunE`：

| 文件 | 命令 | 当前签名 | 改后签名 |
|---|---|---|---|
| `cmd/sclient/cd.go` | cdCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/cd.go` | pwdCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/cd.go` | mkdirCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/cd.go` | rmdirCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/config.go` | configCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/upload.go` | uploadCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/download.go` | downloadCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/delete.go` | deleteCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/list.go` | listCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/search.go` | searchCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/stat.go` | statCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/mv.go` | mvCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/tunnel.go` | tunnelCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/genkey.go` | genkeyCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/version.go` | versionCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/version.go` | versionListCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/version.go` | versionRestoreCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/version.go` | versionDeleteCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/diag.go` | diagCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/archive.go` | archiveCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/archive.go` | archiveDirCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/batch_delete.go` | batchDeleteCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/batch_rename.go` | batchRenameCmd | `Run: func(...)` | `RunE: func(...) error` |
| `cmd/sclient/relay.go` | relayCmd | `Run: func(...)` | `RunE: func(...) error` |

### 测试并发与安全性提升

| # | 改进点 | 方案 |
|---|---|---|
| 1 | 全局变量 `currentDir` 消除 | 封装为结构体，测试时各自创建实例，实现子测试可以 `t.Parallel()` |
| 2 | 测试中 `currentDir` 串行限制 | 消除后移除 `// 不并行` 注释，启用 `t.Parallel()` |
| 3 | `cfgFile` 测试隔离 | 已用 `t.Cleanup`；验证所有测试路径都被覆盖 |
| 4 | `httptest.Server` 关闭 | 所有 mock server 使用 `defer mock.Close()` 确认 |
| 5 | `cmd.SetArgs` 并发安全 | 串行执行 `rootCmd.SetArgs`，用 `sync.Mutex` 或 `rootCmd.SetArgs` 的独占使用约定 |
| 6 | `captureStdout` 并发安全 | 统一使用 `pkg/testutil.CaptureStdout`（已有 `sync.Mutex` 保护） |

### CLAUDE.md 优化

- 头部核心指引（项目定位、命令、架构、路由、配置、测试规范）保留
- 20 个 skills 列表改为 `<details>` 折叠块（减少高频加载时的视觉噪音）
- 详细 skills 描述抽取到 `docs/SUPERPOWERS.md`
- 新增自动化检查说明（防止测试监听回退到 localhost）

### 测试监听绑定检查

已全面确认：所有测试文件使用 `127.0.0.1:0` 或 `httptest.NewServer`（默认 loopback）。未发现 `0.0.0.0` 或实际监听 `localhost` 的情况。无需修改。在 Makefile 的 test 目标中加入 grep 快速检查作为回归防护。

## 实现计划

### Phase 1：cmd 独立 module

1. 为 `cmd/sproxy/` 创建 `go.mod`：`module github.com/cocomhub/sproxy/cmd/sproxy`，require cobra/viper，replace 指向根 module
2. 为 `cmd/sclient/` 创建 `go.mod`：`module github.com/cocomhub/sproxy/cmd/sclient`，require cobra/viper，replace 指向根 module
3. 更新 `go.work` 的 `use` 指令包含两个新 module
4. 更新 Makefile 的构建命令以适应新位置（cmd module 构建从自身 go.mod 读取）
5. 验证 `go build ./cmd/sproxy/...` 和 `go build ./cmd/sclient/...` 通过

### Phase 2：修复已知技术债务

1. **信号 goroutine 泄漏修复**（`cmd/sproxy/root.go`）：
   - `runServer` 中 `for sig := range signalChan` 改为 select + 检查
   - 在 `ListenAndServe` 和 `Shutdown` 超时后关闭 `signalChan`
   - 或改用 `signal.NotifyContext` + `<-ctx.Done()`

2. **`os.Exit(1)` 消除**（`cmd/sclient/cd.go`, `cmd/sclient/config.go`）：
   - `mustResolveRemotePath` 中的 `os.Exit` 改为返回 error
   - 调用方自行决定是否退出
   - `configCmd` 的错误处理同样改为 error 返回
   - 更新对应测试移除 `t.Skip`

3. **`captureStdout` 去重**：
   - cmd 独立后，删除 `cmd/sproxy/root_test.go` 和 `cmd/sclient/cmd_test.go` 中的私有辅助函数
   - 改为导入 `pkg/testutil.CaptureStdout` 和 `pkg/testutil.CaptureStderr`

4. **`findModuleRoot` 冗余消除**（`test/e2e_test.go`）：
   - 删除 `findModuleRoot` 函数
   - 统一使用已有的 `runtime.Caller` 方案确定项目根路径

5. **`newTestServerWithAllRoutes` 路由同步问题**（`pkg/server/integration_test.go`）：
   - 重构为基于 `RegisterRoutes` 的行为测试，而非手动重复路由列表
   - 或在测试中直接调用 `RegisterRoutes` 后遍历 handler 模式

### Phase 3：viper 全局单例迁移

1. `cmd/sproxy/root.go` 中 `viper.GetViper()` 改为 `viper.New()`
2. `cmd/sclient/root.go` 中 `viper.GetViper()` 改为 `viper.New()`
3. 更新所有依赖全局 viper 变量的引用（注意测试中的 viper 隔离）

### Phase 4：CLAUDE.md 优化

1. 抽取 skills 详细描述到 `docs/SUPERPOWERS.md`
2. 将 `CLAUDE.md` 中的 skills 列表改为 `<details>` 折叠块
3. 在 Makefile test 目标中加入 127.0.0.1 绑定检查

### Phase 5：回归验证

- `go vet ./...`
- `go test -race ./...`（含所有子 module）
- `make build` 确认两个二进制正常构建
- 确认 cover-html 和 test-packages 目标正常工作

## 验证方式

1. **构建验证**：`go build ./cmd/sproxy/` 和 `go build ./cmd/sclient/` 成功
2. **测试验证**：`go test -race ./cmd/sproxy/... ./cmd/sclient/... ./pkg/...` 全部通过
3. **`os.Exit(1)` 消除验证**：`cmd/sclient/cmd_rune_test.go` 中 `TestTunnelCommand_MissingKey` 测试不再需要 Skip
4. **CLAUDE.md 按需加载验证**：折叠后 skills 列表不占用初始阅读空间
