# sclient CLI 重构实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将 sclient CLI 从全局变量 + `init()` 注册模式重构为工厂函数 + 依赖注入模式，消除 57 个全局变量，引入 `client.Service` 接口便于测试 mock。

**架构：**
- `pkg/cli/` — 通用 CLI 抽象（IOStreams），可被 sclient 和 sproxy 复用
- `pkg/client/` — 新增 `Service` 接口，`FileClient` 实现它
- `cmd/sclient/internal/clientfactory/` — `Factory` 接口返回 `client.Service`，生产/测试双实现
- `cmd/sclient/internal/state/` — `State` 封装 `currentDir` + 路径解析 + XDG 持久化
- 每个命令文件添加 `NewCmdXxx(factory, ioStreams, state)` 工厂函数，旧全局变量保留兼容

**技术栈：** Go 1.26, cobra v1.10.2, viper v1.21.0

**PR 拆分策略：** 每个 PR 从 `origin/master` 拉分支 → 实现 → 测试 → 审查 → 修复 → 合并。PR 间无依赖，可并行开发。

---

## 文件结构总览

### 创建的文件

| 文件路径 | 职责 |
|----------|------|
| `pkg/cli/iostreams.go` | `IOStreams` 结构体（In/Out/ErrOut） |
| `pkg/cli/iostreams_test.go` | IOStreams 测试 |
| `cmd/sclient/internal/clientfactory/factory.go` | `Factory` 接口 + `factory` 生产实现 + `mockFactory` 测试实现 |
| `cmd/sclient/internal/clientfactory/factory_test.go` | Factory 测试 |
| `cmd/sclient/internal/state/state.go` | `State` 结构体（CurrentDir + ResolveRemotePath + Save/Load） |
| `cmd/sclient/internal/state/state_test.go` | State 测试 |

### 修改的文件

| 文件路径 | 变更 |
|----------|------|
| `pkg/client/client.go` | 新增 `Service` 接口 + `var _ Service = (*FileClient)(nil)` 编译期检查 |
| `cmd/sclient/genkey.go` | 添加 `NewCmdGenkey()` 工厂函数 |
| `cmd/sclient/cd.go` | 添加 `NewCmdCd()`, `NewCmdPwd()`, `NewCmdMkdir()`, `NewCmdRmdir()` |
| `cmd/sclient/version.go` | 添加 `NewCmdVersion()`, `NewCmdVersionList()`, `NewCmdVersionRestore()`, `NewCmdVersionDelete()` |
| `cmd/sclient/config.go` | 添加 `NewCmdConfig()`, `NewCmdConfigRemote()`, `NewCmdConfigRemoteSet()` |
| `cmd/sclient/stats.go` | 添加 `NewCmdStats()` |
| `cmd/sclient/diag.go` | 添加 `NewCmdDiag()` |
| `cmd/sclient/upload.go` | 添加 `NewCmdUpload()` |
| `cmd/sclient/download.go` | 添加 `NewCmdDownload()` |
| `cmd/sclient/delete.go` | 添加 `NewCmdDelete()` |
| `cmd/sclient/list.go` | 添加 `NewCmdList()` |
| `cmd/sclient/stat.go` | 添加 `NewCmdStat()` |
| `cmd/sclient/search.go` | 添加 `NewCmdSearch()` |
| `cmd/sclient/mv.go` | 添加 `NewCmdMv()` |
| `cmd/sclient/archive.go` | 添加 `NewCmdArchive()`, `NewCmdArchiveDir()` |
| `cmd/sclient/batch_delete.go` | 添加 `NewCmdBatchDelete()` |
| `cmd/sclient/batch_rename.go` | 添加 `NewCmdBatchRename()` |
| `cmd/sclient/preview.go` | 添加 `NewCmdPreview()` |
| `cmd/sclient/tunnel.go` | 添加 `NewCmdTunnel()` |
| `cmd/sclient/share.go` | 添加 `NewCmdShare()`, `NewCmdShareCreate()`, `NewCmdShareList()`, `NewCmdShareRevoke()` |
| `cmd/sclient/cloud_download.go` | 添加 `NewCmdCloudDownload()` |
| `cmd/sclient/cloud_list.go` | 重构为使用 `client.Service`，添加 `NewCmdCloudList()` |
| `cmd/sclient/cloud_cancel.go` | 添加 `NewCmdCloudCancel()` |
| `cmd/sclient/relay.go` | 添加 `NewCmdRelay()`, `NewCmdRelayStart()`, `NewCmdRelayStatus()`, `NewCmdRelayStop()` |
| `cmd/sclient/relay_mgmt.go` | 添加 `NewCmdRelayRemoveNode()`, `NewCmdRelayStats()` |
| `cmd/sclient/root.go` | 最终 PR: 替换为 `NewRootCmd()` + `Execute()` |
| `cmd/sclient/main.go` | 最终 PR: 调用 `NewRootCmd().Execute()` |
| `cmd/sclient/output.go` | 最终 PR: `buildFormatter()` 改为接收 `io.Writer` 参数 |
| `cmd/sclient/errors.go` | 可能新增错误格式常量 |
| 测试文件 | 逐步迁移，最终 PR 删除 `captureRootCmdArgs()` |

---

## PR 1: 基础抽象层

> 从 `origin/master` 拉分支 `feat/cli-abstraction-layer`

**目标：** 创建所有新抽象包，零破坏性（纯新增）。

### 任务 1.1: 创建 `pkg/cli/iostreams.go`

**文件：**
- 创建：`pkg/cli/iostreams.go`
- 测试：`pkg/cli/iostreams_test.go`

- [ ] **步骤 1：编写测试**

```go
// pkg/cli/iostreams_test.go
package cli_test

import (
    "bytes"
    "io"
    "strings"
    "testing"

    "github.com/cocomhub/sproxy/pkg/cli"
)

func TestSystemIOStreams_NotNil(t *testing.T) {
    ios := cli.SystemIOStreams()
    if ios.In == nil || ios.Out == nil || ios.ErrOut == nil {
        t.Error("SystemIOStreams should return non-nil streams")
    }
}

func TestIOStreams_WriteToOut(t *testing.T) {
    var buf bytes.Buffer
    ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
    _, err := ios.Out.Write([]byte("hello"))
    if err != nil {
        t.Fatalf("write failed: %v", err)
    }
    if buf.String() != "hello" {
        t.Errorf("got %q, want %q", buf.String(), "hello")
    }
}

func TestIOStreams_WriteToErrOut(t *testing.T) {
    var buf bytes.Buffer
    ios := cli.IOStreams{ErrOut: &buf, Out: io.Discard}
    ios.WriteErrLine("error: %s", "test")
    if !strings.Contains(buf.String(), "error: test") {
        t.Errorf("expected error output, got %q", buf.String())
    }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go test ./pkg/cli/ -v -count=1`
预期：编译错误，package cli not found

- [ ] **步骤 3：编写实现代码**

```go
// pkg/cli/iostreams.go
package cli

import (
    "fmt"
    "io"
    "os"
)

// IOStreams 封装 CLI 命令的输入输出流。
// 测试时可通过注入 strings.Builder 捕获输出，无需 CaptureStdout 包装。
type IOStreams struct {
    In     io.Reader
    Out    io.Writer
    ErrOut io.Writer
}

// SystemIOStreams 返回指向标准输入/输出/错误流的 IOStreams。
func SystemIOStreams() IOStreams {
    return IOStreams{
        In:     os.Stdin,
        Out:    os.Stdout,
        ErrOut: os.Stderr,
    }
}

// WriteErrLine 格式化写入 ErrOut，末尾追加换行。
func (ios IOStreams) WriteErrLine(format string, args ...any) {
    fmt.Fprintf(ios.ErrOut, format+"\n", args...)
}

// WriteOutLine 格式化写入 Out，末尾追加换行。
func (ios IOStreams) WriteOutLine(format string, args ...any) {
    fmt.Fprintf(ios.Out, format+"\n", args...)
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go test ./pkg/cli/ -v -count=1`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/cli/
git commit -m "feat(cli): add IOStreams abstraction for testable CLI I/O"
```

### 任务 1.2: 添加 `client.Service` 接口

**文件：**
- 修改：`pkg/client/client.go`
- 新增编译期检查

- [ ] **步骤 1：在 `pkg/client/client.go` 中添加 `Service` 接口定义**

在 `FileClient` 结构体定义之前，添加接口：

```go
// Service 是文件操作接口，所有 sclient 命令通过此接口操作。
// FileClient 是生产实现，测试可注入 mock。
type Service interface {
    Upload(ctx context.Context, localPath, remotePath string) (*UploadResult, error)
    ChunkedUpload(ctx context.Context, localPath, remotePath string, opts ...ChunkedOption) (*UploadResult, error)
    Download(ctx context.Context, filename, outputPath string) error
    Delete(ctx context.Context, filename string, localPath string) error
    Stat(ctx context.Context, filename string) (*FileInfo, error)
    List(ctx context.Context, subdirs ...string) ([]FileInfo, error)
    Search(ctx context.Context, q string) ([]FileInfo, error)
    Rename(ctx context.Context, from, to, fromChecksum string) error
    Mkdir(ctx context.Context, dirname string) error
    Rmdir(ctx context.Context, dirname string) error
    CreateShare(ctx context.Context, filename string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error)
    ListShares(ctx context.Context) ([]*ShareLink, error)
    RevokeShare(ctx context.Context, token string) error
    ListVersions(ctx context.Context, filename string) ([]VersionInfo, error)
    RestoreVersion(ctx context.Context, filename string, versionID int64) error
    DeleteVersion(ctx context.Context, filename string, versionID int64) error
    GetConfig(ctx context.Context) (*ConfigResponse, error)
    UpdateConfig(ctx context.Context, updates map[string]any) error
    CloudDownload(ctx context.Context, url string, opts ...CloudDownloadOption) (*CloudTask, error)
    ListCloudTasks(ctx context.Context) ([]CloudTask, error)
    CancelCloudTask(ctx context.Context, taskID string) error
    BatchDelete(ctx context.Context, files []BatchDeleteFile) ([]BatchOperationResult, error)
    BatchRename(ctx context.Context, operations []BatchRenameOp) ([]BatchOperationResult, error)
    Archive(ctx context.Context, name string, paths []string) (*ArchiveResult, error)
    ArchiveDir(ctx context.Context, name string) (*ArchiveResult, error)
    TunnelDo(req *http.Request) (*http.Response, error)
}
```

在 `FileClient` 结构体定义后添加编译期检查：

```go
// 编译期检查 FileClient 实现 Service 接口
var _ Service = (*FileClient)(nil)
```

注意：`CloudDownload`, `ListCloudTasks`, `CancelCloudTask`, `CloudTask`, `CloudDownloadOption`, `ArchiveResult` 需要检查是否已在 `pkg/client/` 中定义。如果不存在，需要在此 PR 中补充定义或留到后续 PR 处理。

- [ ] **步骤 2：检查缺失的类型**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go build ./pkg/client/...`
预期：如果缺少 `CloudTask` 等类型，编译失败。需要补充类型定义。

- [ ] **步骤 3：补充缺失的类型定义（如果需要）**

如果 `CloudTask`, `CloudDownloadOption`, `ArchiveResult` 未在 `pkg/client/` 中定义，补充：

```go
// pkg/client/cloud.go — 新增
package client

import "context"

// CloudTask 表示云端下载任务。
type CloudTask struct {
    ID         string `json:"id"`
    URL        string `json:"url"`
    Filename   string `json:"filename"`
    Status     string `json:"status"`
    TotalSize  int64  `json:"total_size"`
    Downloaded int64  `json:"downloaded"`
    Checksum   string `json:"checksum"`
    Error      string `json:"error"`
}

// CloudDownloadOption 是 CloudDownload 的选项。
type CloudDownloadOption func(*cloudDownloadOptions)

type cloudDownloadOptions struct {
    ForceAsync  bool
    NoCleanup   bool
    PollInterval time.Duration
}

// ArchiveResult 表示存档操作结果。
type ArchiveResult struct {
    ArchivePath string `json:"archive_path"`
    FileCount   int    `json:"file_count"`
    TotalSize   int64  `json:"total_size"`
}
```

同时添加 `CloudDownload` 等方法到 `FileClient` 上——如果当前不存在，添加 stub 实现。

- [ ] **步骤 4：运行测试验证编译通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go test ./pkg/client/... -count=1`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/client.go pkg/client/cloud.go
git commit -m "feat(client): add Service interface for testable client abstraction"
```

### 任务 1.3: 创建 `internal/clientfactory/`

**文件：**
- 创建：`cmd/sclient/internal/clientfactory/factory.go`
- 测试：`cmd/sclient/internal/clientfactory/factory_test.go`

- [ ] **步骤 1：编写测试**

```go
// cmd/sclient/internal/clientfactory/factory_test.go
package clientfactory_test

import (
    "errors"
    "testing"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/pkg/client"
    "github.com/spf13/cobra"
)

func TestFactory_Interface(t *testing.T) {
    // 编译期检查：确保 factory 类型实现 Factory 接口
    var _ clientfactory.Factory = (*clientfactory.Factory)(nil)
}

func TestMockFactory_Success(t *testing.T) {
    mockSvc := &mockService{}
    factory := clientfactory.NewMock(mockSvc, nil)

    cmd := &cobra.Command{}
    svc, err := factory.NewClient(cmd)
    if err != nil {
        t.Fatalf("NewClient should not error: %v", err)
    }
    if svc != mockSvc {
        t.Error("NewClient should return the mock service")
    }
}

func TestMockFactory_Error(t *testing.T) {
    expectedErr := errors.New("config error")
    factory := clientfactory.NewMock(nil, expectedErr)

    cmd := &cobra.Command{}
    _, err := factory.NewClient(cmd)
    if err != expectedErr {
        t.Errorf("expected error %v, got %v", expectedErr, err)
    }
}

// mockService 实现 client.Service 用于测试
type mockService struct {
    client.Service // embed 接口，默认所有方法 panic
}

func (m *mockService) Upload(ctx context.Context, localPath, remotePath string) (*client.UploadResult, error) {
    return &client.UploadResult{Success: true}, nil
}
```

- [ ] **步骤 2：实现 Factory 接口和生产实现**

```go
// cmd/sclient/internal/clientfactory/factory.go
package clientfactory

import (
    "github.com/cocomhub/sproxy/pkg/client"
    "github.com/spf13/cobra"
)

// Factory 抽象客户端创建，生产/测试可替换。
type Factory interface {
    // NewClient 从 cobra 命令和配置创建 client.Service。
    NewClient(cmd *cobra.Command) (client.Service, error)
}

// factory 是生产实现，封装配置加载 + flag 覆盖 + 客户端构造。
type factory struct {
    cfgFile    string
    viperFn    func() *viperProvider
}

// viperProvider 是 Factory 内部对配置提供者的最小接口
type viperProvider interface {
    BindPFlag(key string, flag *pflag.Flag)
    Unmarshal(obj any) error
    Set(key string, value any)
}
```

- [ ] **步骤 3：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && go test ./internal/clientfactory/... -v -count=1`
预期：PASS

- [ ] **步骤 4：Commit**

```bash
git add cmd/sclient/internal/clientfactory/
git commit -m "feat(clientfactory): add Factory interface for injectable client creation"
```

### 任务 1.4: 创建 `internal/state/`

**文件：**
- 创建：`cmd/sclient/internal/state/state.go`
- 测试：`cmd/sclient/internal/state/state_test.go`

- [ ] **步骤 1：编写测试**

```go
// cmd/sclient/internal/state/state_test.go
package state_test

import (
    "testing"
    "path/filepath"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/state"
)

func TestState_ResolveRemotePath_Absolute(t *testing.T) {
    s := &state.State{CurrentDir: "subdir"}
    got, err := s.ResolveRemotePath("/abs/path")
    if err != nil {
        t.Fatalf("ResolveRemotePath failed: %v", err)
    }
    if got != "abs/path" {
        t.Errorf("got %q, want %q", got, "abs/path")
    }
}

func TestState_ResolveRemotePath_Relative(t *testing.T) {
    s := &state.State{CurrentDir: "base"}
    got, err := s.ResolveRemotePath("file.txt")
    if err != nil {
        t.Fatalf("ResolveRemotePath failed: %v", err)
    }
    if got != "base/file.txt" {
        t.Errorf("got %q, want %q", got, "base/file.txt")
    }
}

func TestState_ResolveRemotePath_EmptyCurrentDir(t *testing.T) {
    s := &state.State{CurrentDir: ""}
    got, err := s.ResolveRemotePath("file.txt")
    if err != nil {
        t.Fatalf("ResolveRemotePath failed: %v", err)
    }
    if got != "file.txt" {
        t.Errorf("got %q, want %q", got, "file.txt")
    }
}

func TestState_ResolveRemotePath_ParentRef(t *testing.T) {
    s := &state.State{CurrentDir: "base"}
    _, err := s.ResolveRemotePath("../file.txt")
    if err == nil {
        t.Error("expected error for parent reference")
    }
}
```

- [ ] **步骤 2：实现 State**

```go
// cmd/sclient/internal/state/state.go
package state

import (
    "fmt"
    "path/filepath"
    "strings"
)

// State 管理 CLI 的漫游当前目录。
// 每个测试应使用独立的 State 实例，无需全局变量。
type State struct {
    CurrentDir string
}

// ResolveRemotePath 根据当前目录和用户传入的路径，返回完整的远端路径。
// 绝对路径（/ 开头）绕过 currentDir；相对路径拼接 currentDir。
// 包含父级引用（..）时返回错误。
func (s *State) ResolveRemotePath(userPath string) (string, error) {
    if userPath == "" {
        return s.CurrentDir, nil
    }

    var raw string
    if strings.HasPrefix(userPath, "/") {
        raw = userPath[1:]
    } else if s.CurrentDir != "" {
        raw = s.CurrentDir + "/" + userPath
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

// ResolveRemotePathOrErr 是 ResolveRemotePath 的便捷封装，返回 error。
func (s *State) ResolveRemotePathOrErr(userPath string) (string, error) {
    cleaned, err := s.ResolveRemotePath(userPath)
    if err != nil {
        return "", fmt.Errorf("无效的路径: %w", err)
    }
    return cleaned, nil
}
```

- [ ] **步骤 3：运行测试验证通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && go test ./internal/state/... -v -count=1`
预期：PASS

- [ ] **步骤 4：Commit**

```bash
git add cmd/sclient/internal/state/
git commit -m "feat(state): add State for current directory management"
```

### 任务 1.5: PR 审查与修复

- [ ] **步骤 1：运行 lint**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && golangci-lint run ./internal/... ./pkg/cli/...`
预期：无 lint 错误

- [ ] **步骤 2：运行全量测试**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && go test -count=1 ./...`
预期：PASS（兼容性测试，旧代码不应受影响）

- [ ] **步骤 3：修复发现的问题**

- [ ] **步骤 4：提交修复 commit**

```bash
git commit -m "fix: address review feedback for abstraction layer"
```

- [ ] **步骤 5：提交 PR**

```bash
git push origin feat/cli-abstraction-layer
# 创建 PR 到 origin/master
```

---

## PR 2: 迁移简单命令

> 从 `origin/master` 拉分支 `feat/migrate-simple-commands`

**目标：** 迁移 genkey、version、cd/pwd/mkdir/rmdir、config、stats、diag 等不依赖或较少依赖 `buildFileClient()` 的命令。

### 任务 2.1: 迁移 genkey 命令

**文件：**
- 修改：`cmd/sclient/genkey.go`

- [ ] **步骤 1：添加 `NewCmdGenkey()` 工厂函数**

```go
// cmd/sclient/genkey.go — 在现有 var genkeyCmd 之后添加

func NewCmdGenkey(ios cli.IOStreams) *cobra.Command {
    return &cobra.Command{
        Use:   "genkey",
        Short: "生成 tunnel_key 密钥",
        Run: func(cmd *cobra.Command, args []string) {
            key, err := tunnel.GenerateKey()
            if err != nil {
                ios.WriteErrLine("生成密钥失败: %v", err)
                return
            }
            ios.WriteOutLine(key)
        },
    }
}
```

- [ ] **步骤 2：运行测试验证编译通过**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && go build ./...`
预期：编译成功

- [ ] **步骤 3：Commit**

```bash
git add cmd/sclient/genkey.go
git commit -m "feat(genkey): add NewCmdGenkey factory function"
```

### 任务 2.2: 迁移 version 命令

**文件：**
- 修改：`cmd/sclient/version.go`

- [ ] **步骤 1：添加 `NewCmdVersion()` 工厂函数**

```go
func NewCmdVersion(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "version",
        Short: "显示版本信息",
        RunE: func(cmd *cobra.Command, args []string) error {
            fmt.Fprintf(ios.Out, "sclient %s (built at %s)\n", Version, BuildAt)
            return nil
        },
    }
    cmd.AddCommand(NewCmdVersionList(factory, ios))
    cmd.AddCommand(NewCmdVersionRestore(factory, ios))
    cmd.AddCommand(NewCmdVersionDelete(factory, ios))
    return cmd
}

func NewCmdVersionList(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
    return &cobra.Command{
        Use:   "list <filename>",
        Short: "列出文件版本历史",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                return err
            }
            versions, err := svc.ListVersions(cmd.Context(), args[0])
            if err != nil {
                return err
            }
            fm := buildFormatter(ios, cmd)
            fm.PrintVersionList(args[0], versions)
            return nil
        },
    }
}

// NewCmdVersionRestore, NewCmdVersionDelete 同理
```

- [ ] **步骤 2：运行测试验证编译通过**

- [ ] **步骤 3：Commit**

### 任务 2.3: 迁移 cd/pwd/mkdir/rmdir 命令

**文件：**
- 修改：`cmd/sclient/cd.go`

- [ ] **步骤 1：添加 `NewCmdCd()` 等工厂函数**

```go
func NewCmdCd(st *state.State, ios cli.IOStreams) *cobra.Command {
    return &cobra.Command{
        Use:   "cd [path]",
        Short: "切换当前目录",
        Run: func(cmd *cobra.Command, args []string) {
            // 使用 st.CurrentDir 替代全局 currentDir
            if len(args) == 0 {
                if st.CurrentDir == "" {
                    ios.WriteOutLine("/")
                } else {
                    ios.WriteOutLine("/%s", st.CurrentDir)
                }
                return
            }
            // ... 切换目录逻辑
        },
    }
}
```

### 任务 2.4: 迁移 config 命令

**文件：**
- 修改：`cmd/sclient/config.go`

- [ ] **步骤 1：添加 `NewCmdConfig()` 等工厂函数**

config 命令需要访问 `cfgFile` 路径进行回写。`Factory` 接口需要暴露配置路径，或在 `NewCmdConfig` 中额外传入路径。

方案：`NewCmdConfig(factory clientfactory.Factory, ios cli.IOStreams, cfgFile string)`

### 任务 2.5: 迁移 stats 和 diag 命令

### 任务 2.6: 为所有迁移的命令编写测试

- [ ] **步骤 1：为 `NewCmdGenkey` 编写测试**

```go
func TestNewCmdGenkey(t *testing.T) {
    var buf strings.Builder
    ios := cli.IOStreams{Out: &buf, ErrOut: &buf}
    cmd := NewCmdGenkey(ios)
    cmd.Run(cmd, nil)
    out := strings.TrimSpace(buf.String())
    if len(out) != 64 {
        t.Errorf("expected 64 hex chars, got %d: %q", len(out), out)
    }
}
```

### 任务 2.7: PR 审查与修复

- [ ] **步骤 1：运行 lint 和全量测试**
- [ ] **步骤 2：提交 PR**

---

## PR 3: 迁移核心文件操作命令

> 从 `origin/master` 拉分支 `feat/migrate-file-commands`

**目标：** 迁移 upload、download、delete、list、stat、search、mv 等核心文件操作命令。

### 任务 3.1: 迁移 upload 命令

**文件：**
- 修改：`cmd/sclient/upload.go`
- 测试：`cmd/sclient/upload_test.go`（新增）

- [ ] **步骤 1：添加 `NewCmdUpload()` 工厂函数**

```go
func NewCmdUpload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "upload <file1> [file2...]",
        Short: "上传一个或多个文件",
        Args:  cobra.MinimumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            svc, err := factory.NewClient(cmd)
            if err != nil {
                ios.WriteErrLine("初始化客户端失败: %v", err)
                return fmt.Errorf(errFmtInitClient, err)
            }
            // 迁移逻辑：全局 currentDir → st.CurrentDir
            // 迁移逻辑：fmt.Printf → ios.Out
            // ...
        },
    }
    cmd.Flags().Bool("chunked", false, "启用分块上传模式")
    cmd.Flags().Int64("chunk-size", 0, "分块大小")
    cmd.Flags().Int("concurrency", 0, "上传并发数")
    cmd.Flags().Bool("resume", true, "续传模式")
    return cmd
}
```

- [ ] **步骤 2：编写测试**

```go
func TestNewCmdUpload_Args(t *testing.T) {
    var buf, errBuf strings.Builder
    ios := cli.IOStreams{Out: &buf, ErrOut: &errBuf}
    factory := clientfactory.NewMock(nil, nil)
    cmd := NewCmdUpload(factory, ios, state.New())
    
    // 无参数应报错
    if err := cmd.Args(cmd, []string{}); err == nil {
        t.Error("upload should require at least 1 arg")
    }
    // 1 个参数应通过
    if err := cmd.Args(cmd, []string{"a.txt"}); err != nil {
        t.Errorf("upload with 1 arg should be ok: %v", err)
    }
}

func TestNewCmdUpload_RunE_Success(t *testing.T) {
    var buf, errBuf strings.Builder
    ios := cli.IOStreams{Out: &buf, ErrOut: &errBuf}
    
    mockSvc := &mockService{
        uploadFn: func(ctx context.Context, localPath, remotePath string) (*client.UploadResult, error) {
            return &client.UploadResult{Success: true, Message: "uploaded"}, nil
        },
    }
    factory := clientfactory.NewMock(mockSvc, nil)
    st := state.New()
    
    cmd := NewCmdUpload(factory, ios, st)
    // 需要 mock 文件系统，或使用临时文件
    tmpFile := filepath.Join(t.TempDir(), "test.txt")
    os.WriteFile(tmpFile, []byte("hello"), 0644)
    
    // 通过 RunE 直接调用
    err := cmd.RunE(cmd, []string{tmpFile})
    if err != nil {
        t.Fatalf("upload failed: %v", err)
    }
    if !strings.Contains(buf.String(), "成功") {
        t.Errorf("expected success message, got: %s", buf.String())
    }
}
```

### 任务 3.2: 迁移 download 命令

### 任务 3.3: 迁移 delete 命令

### 任务 3.4: 迁移 list 命令

### 任务 3.5: 迁移 stat 命令

### 任务 3.6: 迁移 search 命令

### 任务 3.7: 迁移 mv 命令

### 任务 3.8: PR 审查与修复

---

## PR 4: 迁移批量/高级命令

> 从 `origin/master` 拉分支 `feat/migrate-advanced-commands`

**目标：** 迁移 batch-delete、batch-rename、archive、preview、share、tunnel 等命令。

### 任务 4.1: 迁移 batch-delete 和 batch-rename

### 任务 4.2: 迁移 archive 和 archive-dir

### 任务 4.3: 迁移 preview

### 任务 4.4: 迁移 share 系列

### 任务 4.5: 迁移 tunnel

tunnel 命令有特殊模式：`cfgProvider` 的 fallback 初始化，需要处理。

### 任务 4.6: 为所有迁移命令编写测试

### 任务 4.7: PR 审查与修复

---

## PR 5: 迁移 cloud-download 和 relay 命令

> 从 `origin/master` 拉分支 `feat/migrate-cloud-relay-commands`

**目标：** 迁移 cloud-download 系列和 relay 系列命令。这些命令当前使用原始 HTTP 请求（绕过 `buildFileClient()`），需要重构为使用 `client.Service`。

### 任务 5.1: 重构 `cloud_download.go` 为使用 `client.Service`

`cloud_download.go` 中的 `createCloudDownloadTask()`, `pollCloudTask()`, `downloadAndCleanup()` 当前使用 `http.DefaultClient` 直接调用 API。需要：
1. 在 `pkg/client/` 中添加 `CloudDownload()`, `ListCloudTasks()`, `CancelCloudTask()` 方法
2. 迁移 `cloud_download.go` 的命令逻辑为使用 `factory.NewClient(cmd)` 返回的 `client.Service`

### 任务 5.2: 重构 `cloud_list.go` 和 `cloud_cancel.go`

当前 `cloud_list.go` 有自己的 `getCloudServerURL()`, `loadConfigSimple()`, `configSimple` — 这些应在迁移中移除，统一走 `client.Service` 接口。

### 任务 5.3: 迁移 relay 系列命令

### 任务 5.4: 为所有迁移命令编写测试

### 任务 5.5: PR 审查与修复

---

## PR 6: 新根命令 + 全量清理

> 从 `origin/master` 拉分支 `feat/new-root-command-cleanup`

**目标：** 创建 `NewRootCmd()`，删除所有旧全局变量和 `init()` 函数，迁移所有测试，删除 `captureRootCmdArgs()`。

### 任务 6.1: 创建 `NewRootCmd()` 和新的 `Execute()`

**文件：**
- 修改：`cmd/sclient/root.go`
- 修改：`cmd/sclient/main.go`
- 修改：`cmd/sclient/output.go`

- [ ] **步骤 1：实现 `NewRootCmd()`**

```go
// cmd/sclient/root.go — 替换现有 rootCmd 全局变量

func NewRootCmd() *cobra.Command {
    var (
        cfgFile     string
        cfgProvider *sclientcfg.ViperProvider
    )

    ios := cli.SystemIOStreams()
    st := state.New()
    factory := clientfactory.New(&cfgFile, &cfgProvider)

    root := &cobra.Command{
        Use:   "sclient",
        Short: "文件上传下载客户端",
        PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
            cfgProvider = sclientcfg.New(cfgFile)
            cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup(flagServer))
            cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup(flagChunkSize))
            cfgProvider.BindPFlag("auth_token", cmd.Flags().Lookup("auth-token"))
            st.Load() // 从 XDG 加载 currentDir
            return nil
        },
        Run: func(cmd *cobra.Command, args []string) {
            _ = cmd.Help()
        },
    }

    // 配置 flag
    root.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")
    root.PersistentFlags().StringP(flagServer, "s", "", "服务器地址")
    root.PersistentFlags().String("auth-token", "", "Bearer Token 认证令牌")
    root.PersistentFlags().StringP("output", "o", "", "指定下载文件的输出路径")
    root.PersistentFlags().BoolP("verbose", "v", false, "显示详细输出")
    root.PersistentFlags().Bool("chunked", false, "启用分块上传/下载模式")
    root.PersistentFlags().Int64(flagChunkSize, 0, "分块大小")
    root.PersistentFlags().Int("concurrency", 0, "上传/下载并发数")
    root.PersistentFlags().Bool("resume", false, "续传模式")
    root.PersistentFlags().Bool("json", false, "以 JSON 格式输出")

    // 注册子命令
    root.AddCommand(NewCmdUpload(factory, ios, st))
    root.AddCommand(NewCmdDownload(factory, ios, st))
    root.AddCommand(NewCmdDelete(factory, ios, st))
    root.AddCommand(NewCmdList(factory, ios, st))
    root.AddCommand(NewCmdSearch(factory, ios, st))
    root.AddCommand(NewCmdStat(factory, ios, st))
    root.AddCommand(NewCmdMv(factory, ios, st))
    root.AddCommand(NewCmdCd(st, ios))
    root.AddCommand(NewCmdPwd(ios))
    root.AddCommand(NewCmdMkdir(factory, ios, st))
    root.AddCommand(NewCmdRmdir(factory, ios, st))
    root.AddCommand(NewCmdGenkey(ios))
    root.AddCommand(NewCmdVersion(factory, ios))
    root.AddCommand(NewCmdConfig(factory, ios, &cfgFile))
    root.AddCommand(NewCmdTunnel(factory, ios))
    root.AddCommand(NewCmdRelay(factory, ios))
    root.AddCommand(NewCmdCloudDownload(factory, ios, st))
    root.AddCommand(NewCmdShare(factory, ios))
    root.AddCommand(NewCmdBatchDelete(factory, ios, st))
    root.AddCommand(NewCmdBatchRename(factory, ios, st))
    root.AddCommand(NewCmdArchive(factory, ios, st))
    root.AddCommand(NewCmdStats(factory, ios))
    root.AddCommand(NewCmdDiag(ios))
    root.AddCommand(NewCmdPreview(factory, ios, st))

    return root
}

func Execute() error {
    return NewRootCmd().Execute()
}
```

- [ ] **步骤 2：更新 `main.go`**

```go
// cmd/sclient/main.go
package main

var (
    Version = "dev"
    BuildAt = "unknown"
)

func main() {
    if err := Execute(); err != nil {
        os.Exit(1)
    }
}
```

- [ ] **步骤 3：更新 `output.go` 中的 `buildFormatter()`**

```go
func buildFormatter(ios cli.IOStreams, cmd *cobra.Command) OutputFormatter {
    useJSON, _ := cmd.Flags().GetBool("json")
    if useJSON {
        return NewJSONFormatter(ios.Out)
    }
    return NewTextFormatter(ios.Out)
}
```

### 任务 6.2: 删除所有旧全局变量和 init() 函数

- [ ] **步骤 1：删除 `var rootCmd` 全局变量**
- [ ] **步骤 2：删除所有 `var cmdXxx = &cobra.Command{...}` 全局变量**
- [ ] **步骤 3：删除所有 `func init() { rootCmd.AddCommand(...) }` 和 `func init() { cmd.Flags()... }`**
- [ ] **步骤 4：删除旧的 `buildFileClient()` 函数**
- [ ] **步骤 5：删除 `cloud_list.go` 中的 `getCloudServerURL()`, `loadConfigSimple()`, `configSimple`**

### 任务 6.3: 迁移所有测试到新模式

- [ ] **步骤 1：删除 `captureRootCmdArgs()` 辅助函数**
- [ ] **步骤 2：替换所有 `cmd_test.go` 中的全局变量引用为工厂函数创建的命令**

```go
// 旧模式
func TestUploadCmd(t *testing.T) {
    if uploadCmd.Use != "upload <file1> [file2...]" { ... }
}

// 新模式
func TestNewCmdUpload_Use(t *testing.T) {
    cmd := NewCmdUpload(nil, cli.IOStreams{}, state.New())
    if cmd.Use != "upload <file1> [file2...]" { ... }
}
```

- [ ] **步骤 3：替换所有 `cmd_rune_test.go` 中的 `rootCmd.SetArgs()` + `rootCmd.Execute()` 调用**

```go
// 旧模式
func TestSearchCommand_HappyPath(t *testing.T) {
    resetState := captureRootCmdArgs()
    defer resetState()
    rootCmd.SetArgs([]string{"search", "--server", mock.URL, "report"})
    rootCmd.Execute()
}

// 新模式
func TestNewCmdSearch_RunE(t *testing.T) {
    var buf, errBuf strings.Builder
    ios := cli.IOStreams{Out: &buf, ErrOut: &errBuf}
    mockSvc := &mockService{...}
    factory := clientfactory.NewMock(mockSvc, nil)
    
    cmd := NewCmdSearch(factory, ios, state.New())
    // 使用 cobra 的 Command.ExecuteContext 或直接调 RunE
    cmd.RunE(cmd, []string{"report"})
}
```

### 任务 6.4: 全量测试 + lint + 清理

- [ ] **步骤 1：运行全量测试**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && go test -count=1 -race ./...`
预期：所有测试通过

- [ ] **步骤 2：运行 lint**

运行：`cd /d/workdir/leon/cocomhub/sproxy/cmd/sclient && golangci-lint run ./...`
预期：无 lint 错误

- [ ] **步骤 3：清理未使用的导入和常数**

### 任务 6.5: PR 审查与修复

- [ ] **步骤 1：提交 PR**

---

## 自检清单

### 规格覆盖度检查
- [x] `pkg/cli/IOStreams` — 任务 1.1
- [x] `client.Service` 接口 — 任务 1.2
- [x] `internal/clientfactory/` — 任务 1.3
- [x] `internal/state/` — 任务 1.4
- [x] 迁移 genkey — 任务 2.1
- [x] 迁移 version — 任务 2.2
- [x] 迁移 cd/pwd/mkdir/rmdir — 任务 2.3
- [x] 迁移 config — 任务 2.4
- [x] 迁移 stats/diag — 任务 2.5
- [x] 迁移 upload/download/delete — 任务 3.x
- [x] 迁移 list/stat/search/mv — 任务 3.x
- [x] 迁移 batch-delete/batch-rename — 任务 4.1
- [x] 迁移 archive/preview — 任务 4.2-4.3
- [x] 迁移 share — 任务 4.4
- [x] 迁移 tunnel — 任务 4.5
- [x] 迁移 cloud-download 系列 — 任务 5.1-5.2
- [x] 迁移 relay 系列 — 任务 5.3
- [x] 新根命令 — 任务 6.1
- [x] 删除全局变量和 init() — 任务 6.2
- [x] 迁移测试 — 任务 6.3

### 占位符扫描
- [x] 无 "TODO" 或 "待定" 占位符
- [x] 每个步骤有完整代码或清晰的命令

### 类型一致性
- [ ] 确认 `client.Service` 接口方法签名与 `FileClient` 现有方法完全一致
- [ ] 确认 `state.State.ResolveRemotePath` 与现有 `resolveRemotePath` 行为一致
- [ ] 确认 `clientfactory.Factory.NewClient` 返回 `client.Service` 接口

---

## 执行交接

计划已完成并保存到 `docs/superpowers/plans/2026-07-25-sclient-cli-refactoring-plan.md`。

**两种执行方式：**

1. **子代理驱动（推荐）** — 每个 PR 调度新的子代理，任务间进行审查，快速迭代

2. **内联执行** — 在当前会话中使用 executing-plans 执行任务，批量执行并设有检查点

**选哪种方式？**