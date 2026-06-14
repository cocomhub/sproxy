# sproxy 全面优化实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 系统性优化 sproxy 项目：cmd 独立 Go module、全局变量结构体化、cobra 全部改用 RunE、消除 os.Exit、提升测试并发性、精简 CLAUDE.md。

**架构：** cmd/sproxy 和 cmd/sclient 拆为独立 Go module（通过 go.work 管理），引入 appState 结构体封装全局状态并解耦 xdg 依赖，所有命令改为 RunE，业务逻辑移到结构体方法。

**技术栈：** Go 1.26, cobra v1.9.1, viper v1.20.1, adrg/xdg v0.5.3, gopkg.in/yaml.v3

---

### 任务 1：创建 cmd/sproxy 独立 go.mod

**文件：**
- 创建：`cmd/sproxy/go.mod`
- 修改：`go.work`
- 验证构建

- [ ] **步骤 1：创建 cmd/sproxy/go.mod**

```go
module github.com/cocomhub/sproxy/cmd/sproxy

go 1.26

require (
    github.com/cocomhub/sproxy v0.0.0-00010101000000-000000000000
    github.com/spf13/cobra v1.9.1
    github.com/spf13/viper v1.20.1
)

replace github.com/cocomhub/sproxy => ../../
```

- [ ] **步骤 2：更新 go.work 加入新 module**

修改 `go.work`，在原 use 指令中添加新 module 路径：

```
go 1.26

use (
    .
    ./cmd/sproxy
    ./cmd/sclient
    ./pkg/tunnel/xfer/ext/ws
    ./pkg/tunnel/xfer/ext/quic
)
```

- [ ] **步骤 3：验证构建**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go build ./cmd/sproxy/...`
预期：构建成功

- [ ] **步骤 4：Commit**

```bash
git add cmd/sproxy/go.mod go.work
git commit -m "feat: 创建 cmd/sproxy 独立 go.mod"
```

---

### 任务 2：创建 cmd/sclient 独立 go.mod

**文件：**
- 创建：`cmd/sclient/go.mod`
- 验证构建

- [ ] **步骤 1：创建 cmd/sclient/go.mod**

```go
module github.com/cocomhub/sproxy/cmd/sclient

go 1.26

require (
    github.com/adrg/xdg v0.5.3
    github.com/cocomhub/sproxy v0.0.0-00010101000000-000000000000
    github.com/spf13/cobra v1.9.1
    github.com/spf13/viper v1.20.1
)

replace github.com/cocomhub/sproxy => ../../
```

注意：`adrg/xdg` 保留在 sclient module 中因为 `xdg.ConfigFile`、`xdg.CacheFile` 在命令中使用。
后续任务将其隔离到 `xdgDirPersister`，但初期保留确保构建通过。

- [ ] **步骤 2：验证构建**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go build ./cmd/sclient/...`
预期：构建成功

- [ ] **步骤 3：清除本地 modcache（可选，如构建失败）**

运行：`go clean -modcache && go build ./cmd/sclient/...`

- [ ] **步骤 4：Commit**

```bash
git add cmd/sclient/go.mod
git commit -m "feat: 创建 cmd/sclient 独立 go.mod"
```

---

### 任务 3：重构 cmd/sclient — appState 结构体 + RunE 迁移

**文件：**
- 修改：`cmd/sclient/root.go` — 添加 appState 结构体、dirPersister 接口、修改 buildFileClient
- 修改：`cmd/sclient/cd.go` — 改为 RunE + appState 方法
- 修改：`cmd/sclient/config.go` — 改为 RunE
- 修改：`cmd/sclient/upload.go` — 改为 RunE
- 修改：`cmd/sclient/download.go` — 改为 RunE
- 修改：`cmd/sclient/delete.go` — 改为 RunE
- 修改：`cmd/sclient/list.go` — 改为 RunE
- 修改：`cmd/sclient/search.go` — 改为 RunE
- 修改：`cmd/sclient/stat.go` — 改为 RunE
- 修改：`cmd/sclient/mv.go` — 改为 RunE
- 修改：`cmd/sclient/tunnel.go` — 改为 RunE
- 修改：`cmd/sclient/genkey.go` — 改为 RunE
- 修改：`cmd/sclient/version.go` — 改为 RunE（3 个子命令）
- 修改：`cmd/sclient/diag.go` — 改为 RunE
- 修改：`cmd/sclient/archive.go` — 改为 RunE、删除死代码
- 修改：`cmd/sclient/batch_delete.go` — 改为 RunE
- 修改：`cmd/sclient/batch_rename.go` — 改为 RunE
- 修改：`cmd/sclient/relay.go` — 改为 RunE
- 修改：`cmd/sclient/cd_test.go` — 移除串行限制、t.Parallel
- 修改：`cmd/sclient/cmd_rune_test.go` — 移除 t.Skip、适配 RunE

**原则：每个命令的修改模式一致，为避免重复，此处给出通用模式，任务按命令分组执行。**

通用模式：
```go
// 旧：
var uploadCmd = &cobra.Command{
    Run: func(cmd *cobra.Command, args []string) {
        cli, err := buildFileClient(cmd)
        if err != nil {
            fmt.Fprintf(os.Stderr, "初始化客户端失败: %v\n", err)
            os.Exit(1)
        }
        // ... 业务逻辑 ...
    },
}

// 新：
var uploadCmd = &cobra.Command{
    RunE: func(cmd *cobra.Command, args []string) error {
        if err := globalState.ExecUpload(cmd, args); err != nil {
            return err
        }
        return nil
    },
}
```

- [ ] **步骤 1：在 cmd/sclient/root.go 中添加 appState 结构体和 dirPersister 接口**

在 root.go 的 import 块后（var 块前）添加：

```go
// dirPersister 抽象 currentDir 持久化，生产用 xdg，测试用 noop
// 所有方法必须返回 error，禁止静默吞错误
type dirPersister interface {
    LoadDir() (string, error)
    SaveDir(dir string) error
}

type noopDirPersister struct{}
func (p *noopDirPersister) LoadDir() (string, error) { return "", nil }
func (p *noopDirPersister) SaveDir(_ string) error   { return nil }

type xdgDirPersister struct{}
func (p *xdgDirPersister) LoadDir() (string, error) {
    cacheDir, err := xdg.CacheFile("sproxy", "current_dir")
    if err != nil {
        return "", fmt.Errorf("获取缓存目录失败: %w", err)
    }
    data, err := os.ReadFile(cacheDir)
    if err != nil {
        if os.IsNotExist(err) {
            return "", nil // 首次使用，文件不存在不是错误
        }
        return "", fmt.Errorf("读取缓存文件失败: %w", err)
    }
    return strings.TrimSpace(string(data)), nil
}
func (p *xdgDirPersister) SaveDir(dir string) error {
    cacheDir, err := xdg.CacheFile("sproxy", "current_dir")
    if err != nil {
        return fmt.Errorf("获取缓存目录失败: %w", err)
    }
    if err := os.MkdirAll(filepath.Dir(cacheDir), 0755); err != nil {
        return fmt.Errorf("创建缓存目录失败: %w", err)
    }
    if err := os.WriteFile(cacheDir, []byte(dir), 0644); err != nil {
        return fmt.Errorf("写入缓存文件失败: %w", err)
    }
    return nil
}

// appState 封装所有包级状态，测试时可创建独立实例
type appState struct {
    cfgFile    string
    currentDir string
    persister  dirPersister
}

func (s *appState) ResolveRemotePath(userPath string) (string, error) {
    if userPath == "" {
        return s.currentDir, nil
    }
    var raw string
    if strings.HasPrefix(userPath, "/") {
        raw = userPath[1:]
    } else if s.currentDir != "" {
        raw = s.currentDir + "/" + userPath
    } else {
        raw = userPath
    }
    cleaned := filepath.ToSlash(filepath.Clean(raw))
    if cleaned == "." {
        cleaned = ""
    }
    if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
        return "", fmt.Errorf("路径包含父级引用 '..'，禁止访问上层目录: %s", userPath)
    }
    return cleaned, nil
}

func (s *appState) SaveCurrentDir() error {
    if s.persister == nil {
        return nil
    }
    return s.persister.SaveDir(s.currentDir)
}

func (s *appState) LoadCurrentDir() error {
    if s.persister == nil {
        return nil
    }
    dir, err := s.persister.LoadDir()
    if err != nil {
        return err
    }
    s.currentDir = dir
    return nil
}
```

将以下 var 块修改为使用 globalState：

```go
var (
    cfgFile    string
    currentDir string
    globalState = &appState{
        persister: &xdgDirPersister{},
    }
)
```

- [ ] **步骤 2：修改 buildFileClient 接收 viper 实例**

```go
func buildFileClient(cmd *cobra.Command, v *viper.Viper) (*client.FileClient, error) {
    cfg, err := client.LoadFromViper(v)
    if err != nil {
        return nil, fmt.Errorf("配置加载失败: %w", err)
    }
    // ... read flags ...
    serverURL := cfg.ServerURL
    // ... rest of buildFileClient ...
}
```

- [ ] **步骤 3：修改 init 函数在 root.go 中初始化**

```go
func init() {
    // ... flags ...
    
    // 初始化当前目录
    globalState.cfgFile = cfgFile
    globalState.LoadCurrentDir()
}
```

- [ ] **步骤 4：修改 cd.go（cdCmd, pwdCmd, mkdirCmd, rmdirCmd）→ 全部 RunE + 移除 os.Exit**

```go
var cdCmd = &cobra.Command{
    Use:   "cd [path]",
    Short: "切换当前目录",
    Args:  cobra.MaximumNArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        return globalState.execCd(args)
    },
}

func (s *appState) execCd(args []string) error {
    if len(args) == 0 {
        if s.currentDir == "" {
            fmt.Println("/")
        } else {
            fmt.Println("/" + s.currentDir)
        }
        return nil
    }
    path := args[0]
    if path == "/" {
        s.currentDir = ""
        s.SaveCurrentDir()
        return nil
    }
    cleaned, err := s.ResolveRemotePath(path)
    if err != nil {
        return err
    }
    // cd dir 需要检查远端是否为目录
    cli, err := buildFileClient(rootCmd, viper.New())
    if err != nil {
        return err
    }
    // 如果路径以 / 结尾或当前有同名目录则尝试直接 set
    if strings.HasSuffix(path, "/") {
        // ...
    }
    s.currentDir = cleaned
    s.SaveCurrentDir()
    return nil
}
```

> 注意：`setArgsSafety` 辅助函数确保 cobra 命令调用的串行安全。但由于每个子命令的 `RunE` 最终在 cobra Execute 中被串行调用，不需要额外加锁。

- [ ] **步骤 5：修改 config.go → RunE**

```go
var configCmd = &cobra.Command{
    Use:   "config [show|set <key> <value>]",
    Short: "配置管理",
    Args:  cobra.ArbitraryArgs,
    RunE: func(cmd *cobra.Command, args []string) error {
        if len(args) == 0 {
            // show config
            v := viper.New()
            v.SetConfigFile(cfgFile)
            v.SetConfigType("yaml")
            v.SetEnvPrefix("SCLIENT")
            v.AutomaticEnv()
            cfg, err := client.LoadFromViper(v)
            if err != nil {
                return fmt.Errorf("配置加载失败: %w", err)
            }
            // ... 打印配置 ...
            return nil
        }
        switch args[0] {
        case "show":
            // ...
        case "set":
            if len(args) < 3 {
                return fmt.Errorf("用法: sclient config set <key> <value>")
            }
            // ... set config ...
        }
        return nil
    },
}
```

- [ ] **步骤 6：逐个修改所有 Run 命令为 RunE**

按照「通用模式」，将以下文件中的所有 `Run:` 改为 `RunE:`：
- upload.go: `globalState.ExecUpload(cmd, args)`
- download.go: `globalState.ExecDownload(cmd, args)`
- delete.go
- list.go
- search.go
- stat.go
- mv.go
- tunnel.go
- genkey.go
- version.go (versionCmd, versionListCmd, versionRestoreCmd, versionDeleteCmd)
- diag.go
- archive.go (archiveCmd, archiveDirCmd → 从 archive.go:98-102 删除 `var _` 死代码)
- batch_delete.go
- batch_rename.go
- relay.go

每个 `RunE` 统一模式：

```go
RunE: func(cmd *cobra.Command, args []string) error {
    v := viper.New()
    // 配置初始化（从 root.go 复制 PersistentPreRunE 中的 viper 初始化逻辑）
    v.SetConfigFile(cfgFile)
    v.SetConfigType("yaml")
    v.SetEnvPrefix("SCLIENT")
    v.AutomaticEnv()
    _ = v.BindPFlag("server_url", cmd.Flags().Lookup("server"))
    // ...
    
    cli, err := buildFileClient(cmd, v)
    if err != nil {
        return err
    }
    // ... 业务逻辑（原样，但 os.Exit 改为 return）...
    return nil
},
```

> 注意：每个子命令 RunE 需要重新初始化 veiper 实例（因为 cobra 不自动传递 PersistentPreRunE 到 RunE）。
> 这一部分会提取为公共辅助函数 `newViper(*cobra.Command) *viper.Viper`。

- [ ] **步骤 7：提取公共 viper 初始化辅助函数到 root.go**

```go
// newViper 为子命令创建独立 viper 实例并初始化配置
func newViper(cmd *cobra.Command) *viper.Viper {
    v := viper.New()
    v.SetConfigFile(cfgFile)
    v.SetConfigType("yaml")
    v.SetEnvPrefix("SCLIENT")
    v.AutomaticEnv()
    _ = v.BindPFlag("server_url", cmd.Flags().Lookup("server"))
    _ = v.BindPFlag("output", cmd.Flags().Lookup("output"))
    _ = v.BindPFlag("verbose", cmd.Flags().Lookup("verbose"))
    _ = v.BindPFlag("chunked", cmd.Flags().Lookup("chunked"))
    _ = v.BindPFlag("chunk_size", cmd.Flags().Lookup("chunk-size"))
    _ = v.BindPFlag("concurrency", cmd.Flags().Lookup("concurrency"))
    _ = v.BindPFlag("resume", cmd.Flags().Lookup("resume"))
    return v
}
```

- [ ] **步骤 8：更新 cd_test.go — 支持并行测试**

```go
func TestResolveRemotePath(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name       string
        currentDir string
        input      string
        want       string
        wantErr    bool
    }{
        {name: "empty root", currentDir: "", input: "", want: "", wantErr: false},
        {name: "absolute path", currentDir: "", input: "/dir/file.txt", want: "dir/file.txt", wantErr: false},
        {name: "relative path", currentDir: "base", input: "file.txt", want: "base/file.txt", wantErr: false},
        // ... 保持原有 test cases ...
    }
    for _, tc := range tests {
        tc := tc
        t.Run(tc.name, func(t *testing.T) {
            t.Parallel()
            s := &appState{currentDir: tc.currentDir}
            got, err := s.ResolveRemotePath(tc.input)
            if tc.wantErr {
                if err == nil {
                    t.Errorf("expected error for %q (currentDir=%q), got nil (returned %q)", tc.input, tc.currentDir, got)
                }
                return
            }
            if err != nil {
                t.Errorf("unexpected error for %q (currentDir=%q): %v", tc.input, tc.currentDir, err)
            }
            if got != tc.want {
                t.Errorf("for %q (currentDir=%q): got %q, want %q", tc.input, tc.currentDir, got, tc.want)
            }
        })
    }
}
```

移除文件顶部的 `// 不并行` 注释。

- [ ] **步骤 9：更新 cmd_rune_test.go — 适配 RunE、移除 t.Skip**

`TestTunnelCommand_MissingKey` 移除 `t.Skip`，改为验证 `Execute()` 返回 error：

```go
func TestTunnelCommand_MissingKey(t *testing.T) {
    resetState := captureRootCmdArgs()
    defer resetState()
    
    rootCmd.SetArgs([]string{"tunnel", "http://example.com"})
    err := rootCmd.Execute()
    if err == nil {
        t.Error("expected error when tunnel_key is missing")
    }
}
```

- [ ] **步骤 10：Commit（批量，以文件修改对应）**

```bash
git add cmd/sclient/
git commit -m "refactor: cmd/sclient 全局状态结构体化、RunE 迁移、消除 os.Exit"
```

---

### 任务 4：重构 cmd/sproxy — 状态结构体化 + signal 泄漏修复

**文件：**
- 修改：`cmd/sproxy/root.go` — sproxyState 结构体、viper.New()、signal 泄漏修复
- 修改：`cmd/sproxy/root_test.go` — 适配 captureStdout 导入
- 修改：`cmd/sproxy/root_extra_test.go` — 适配 captureStdout 导入

- [ ] **步骤 1：在 root.go 中引入 sproxyState 结构体**

```go
type sproxyState struct {
    cfgFile              string
    cfgPtr               atomic.Pointer[server.Config]
    currentTunnelKeyHex  string
    testSignalCh         chan os.Signal
}

var globalState = &sproxyState{}
```

修改现有 var 块：
```go
var (
    cfgFile    string
    globalState = &sproxyState{}
)
```

将 `PersistentPreRunE` 中的 `viper.GetViper()` 改为 `viper.New()`：

```go
PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
    v := viper.New()
    v.SetConfigFile(cfgFile)
    v.SetConfigType("yaml")
    v.SetEnvPrefix("SPROXY")
    v.AutomaticEnv()
    // ... BindPFlag calls ...
},
```

- [ ] **步骤 2：修复信号 goroutine 泄漏**

将现有的 `signal.Notify` + `for range signalChan` 改为 `signal.NotifyContext`：

```go
func runServer(cmd *cobra.Command, args []string) error {
    // ...
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)
    defer stop()
    
    // ... server setup ...
    
    go func() {
        <-ctx.Done()
        // 不 return，由 stop() 释放 goroutine
        shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ServerTimeouts.Shutdown)
        defer cancel()
        if err := s.Shutdown(shutdownCtx); err != nil {
            slog.Warn("server shutdown error", "error", err)
        }
    }()
    
    // ... ListenAndServe ...
}
```

- [ ] **步骤 3：修改 handleSighup 为结构体方法**

```go
func (s *sproxyState) handleSighup(oldCfg *server.Config, tunUpdater server.TunnelUpdater) {
    v := viper.New()
    v.SetConfigFile(s.cfgFile)
    v.SetConfigType("yaml")
    v.SetEnvPrefix("SPROXY")
    v.AutomaticEnv()
    // ...
}
```

- [ ] **步骤 4：修改 `resolveTunnelKey` 接收 cfgFile 参数**

```go
func resolveTunnelKey(cfg *server.Config, cfgFile string) (string, error) {
    // ...
    return hex.EncodeToString(key[:]), nil
}
```

- [ ] **步骤 5：删除 `cmd/sproxy/root_test.go` 中的 captureStderr 私有实现**

删除 `root_test.go:140-152` 的 `captureStderr` 函数，改为：
```go
import (
    "github.com/cocomhub/sproxy/pkg/testutil"
    // ...
)

// 在测试中：
output := testutil.CaptureStderr(func() { ... })
```

- [ ] **步骤 6：删除 `cmd/sproxy/root_extra_test.go` 中的 captureStdout 私有实现**

删除 `root_extra_test.go:224-245` 的 `captureStdout` 函数，改为使用 `pkg/testutil.CaptureStdout`。

- [ ] **步骤 7：Commit**

```bash
git add cmd/sproxy/
git commit -m "refactor: cmd/sproxy 状态结构体化、signal.NotifyContext、viper.New、capture 统一"
```

---

### 任务 5：修复 pkg/testutil 和消除 test/e2e findModuleRoot 冗余

**文件：**
- 修改：`test/e2e_test.go` — 删除 findModuleRoot

- [ ] **步骤 1：删除 test/e2e_test.go 中的 findModuleRoot 函数**

删除 `findModuleRoot` 函数及其所有引用，使用 `runtime.Caller` 方案（已有 `startSPROXY` 使用）。

验证：找到 `startSPROXY` 或测试中直接引用 `findModuleRoot` 的代码行，替换为 `runtime.Caller(0)` 方式。

- [ ] **步骤 2：Commit**

```bash
git add test/e2e_test.go
git commit -m "refactor: 删除 test/e2e 中冗余的 findModuleRoot，统一 runtime.Caller"
```

---

### 任务 6：更新 Makefile 和回归验证

**文件：**
- 修改：`Makefile`
- 修改：`CLAUDE.md`

- [ ] **步骤 1：更新 Makefile 构建目标适配 cmd module**

```makefile
# 修改 build-% 目标
build-%:
ifeq ($(filter $*,$(CMD_NAMES)),$*)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o $(BIN_DIR)/$*$(EXE) $(GO_LDFLAGS) ./cmd/$*
else
	@echo "Unknown command: $* (available: $(CMD_NAMES))"
	exit 1
endif
```

注意：使用 `./cmd/$*` 而非 `./cmd/$*/...`，前者只构建该 cmd module 本身（不含子包）。
`go build` 会在 cmd module 中找到其 go.mod 并 resolve 依赖。

- [ ] **步骤 2：在 Makefile test 目标前加入 127.0.0.1 绑定检查**

```makefile
.PHONY: test
test: vet check-loopback
	# ... existing test targets ...

.PHONY: check-loopback
check-loopback:
	@echo "=== 检查测试监听地址 ==="
	@! grep -rE --include='*_test.go' '0\.0\.0\.0|localhost:[0-9]' . || { echo "错误: 发现测试文件含 0.0.0.0 或 localhost 监听地址！"; exit 1; }
	@echo "OK"
```

- [ ] **步骤 3：更新 test 目标加入 cmd module 测试**

```makefile
test: vet check-loopback
	@echo "=== cmd/sproxy/... ==="
	go test -race -count=1 -timeout=60s ./cmd/sproxy/... 2>&1
	@echo "=== cmd/sclient/... ==="
	go test -race -count=1 -timeout=60s ./cmd/sclient/... 2>&1
	@echo "=== pkg/... ==="
	go test -race -count=1 -timeout=120s ./pkg/... 2>&1
	@echo "=== test/... ==="
	go test -race -count=1 -timeout=120s ./test/... 2>&1
```

- [ ] **步骤 4：精简 CLAUDE.md — skills 列表改为折叠块**

将 CLAUDE.md 中的 superpowers-zh skills 列表（20 个 skills 的详细描述块）改为：

```markdown
<details>
<summary>Superpowers-ZH 中文增强版 Skills（共 20 个）</summary>

本项目已安装 superpowers-zh 技能框架（位于 `.claude/skills/`）。

- **brainstorming**: 在任何创造性工作之前必须使用此技能 ...
- **chinese-code-review**: 中文 review 沟通参考 ...
...
</details>
```

将核心规则（核心规则 4 条）保留在折叠块外。

- [ ] **步骤 5：全量回归验证**

运行：
```bash
cd /d/workdir/leon/cocomhub/sproxy
make build
make test
```

预期：
- `make build`：成功产出 `build/bin/sproxy` 和 `build/bin/sclient`
- `make test`：所有测试通过，无 race
- `go vet ./...`：通过

- [ ] **步骤 6：Commit 并最终验证**

```bash
git add Makefile CLAUDE.md
git commit -m "chore: 更新 Makefile 适配 cmd module，精简 CLAUDE.md"
```

---

### 任务 7：最终验证与测试通过确认

- [ ] **步骤 1：运行全部测试**

```bash
cd /d/workdir/leon/cocomhub/sproxy
go test -race -count=1 -timeout=180s ./cmd/sproxy/... ./cmd/sclient/... ./pkg/... ./test/... 2>&1
```

- [ ] **步骤 2：验证无 skip 的测试全部运行**

```bash
go test -v -count=1 -timeout=60s ./cmd/sclient/... 2>&1 | grep -E 'SKIP|FAIL|---'
```

确认 `TestTunnelCommand_MissingKey` 不再 SKIP。

- [ ] **步骤 3：验证覆盖率无大幅下降**

```bash
go test -count=1 -coverprofile=build/coverage/cover.out ./pkg/... 2>&1 | tail -1
```

- [ ] **步骤 4：变更总结**

输出所有修改的文件汇总，确认没有遗漏。

---

### 注意事项

1. **cmd 独立 module 后的 import 路径变化**：根 module 的包仍通过 `github.com/cocomhub/sproxy/pkg/...` 引用，无变化
2. **go.work vs 无 work 模式**：`go build ./cmd/sproxy` 在 go.work 下自动 resolve replace 指令；无 work 时需 `cd cmd/sproxy && go build`
3. **`rootCmd.SetArgs` 线程安全**：cobra 的 `SetArgs` 不是线程安全的，但测试中串行调用 `Execute()` 是安全的——cobra 内部 `execute` 在完成前不会返回
4. **`PersistentPreRunE` 与 `RunE` 的关系**：设置 `PersistentPreRunE` 后，`RunE` 不会自动执行它——但移除 `PersistentPreRunE` 改用子命令自己的 `RunE` 初始化 viper 后，完全消除了对全局 `viper.GetViper()` 的依赖
5. **`buildFileClient` 接收 `*viper.Viper` 参数**：所有调用处需同步修改
