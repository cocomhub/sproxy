# sproxy 测试体系全面改进规格说明

## 概述

### 目标

针对 sproxy 项目当前 **70.1%** 语句覆盖率，通过三阶段增量改进（基础设施提取 → 缺口填补 → 难覆盖分析），将覆盖率提升至 **≥75%**，并建立可持续的测试基础设施。

### 现状

| 包 | 当前覆盖率 | 目标 | 关键缺口 |
|---|---|---|---|
| `cmd/sclient` | 24.4% | ≥50% | 16 子命令缺 cobra RunE 测试 |
| `cmd/sproxy` | 46.1% | ≥65% | SIGHUP/密钥/日志 0% |
| `pkg/server` | 73.0% | ≥78% | parsePagination/dirs/deleteVersion/hub |
| `pkg/client` | 73.0% | ≥80% | calcChunkSize/chunked 错误路径 |
| `pkg/tunnel` | 79.8% | ≥85% | UpdateKey/panic/502 |
| `internal/shortid` | 0% | ≥80% | 无测试文件 |
| **整体** | **70.1%** | **≥75%** | |

## 约束

### 测试依赖

- **纯标准库**：不使用 testify、gomega、gomock 等第三方断言/模拟库，延续现有 `t.Fatalf`/`t.Errorf` 模式
- **`gopkg.in/yaml.v3`**：仅配置解析测试需要，直接从 Viper 加载
- **`github.com/spf13/viper`**：测试中优先使用 `viper.New()`（独立实例），避免 `GetViper()` 全局单例污染

### 网络与平台

- **127.0.0.1 回环绑定**：所有含 HTTP 服务的测试必须监听 `127.0.0.1`（`httptest.NewServer` 默认行为即 loopback），**禁止**监听 `0.0.0.0` 或 `localhost`（后者在 Windows 可能触发防火墙授权弹窗）
- **Windows 兼容**：所有测试必须在 Windows 上通过（除标注 `//go:build !windows` 的 Unix-only 测试外）。路径分隔符用 `filepath.Join` / `filepath.ToSlash` 处理跨平台差异
- **端口分配**：优先使用 `httptest.NewServer`（自动分配端口），或 `net.Listen("tcp", "127.0.0.1:0")` + 关闭再使用端口

### 代码风格

- **最小改动**：不重构现有测试，仅在现有文件新增测试或创建新测试文件
- **`_test.go` 同包/外包混合**：延续现有模式：
  - `package server` 测试（白盒）：可访问未导出函数
  - `package server_test` 测试（黑盒）：仅测试公开 API
- **SPDX 许可证头**：所有测试文件必须携带 `Copyright 2026 The Cocomhub Authors. All rights reserved.` / `SPDX-License-Identifier: Apache-2.0`

## 架构决策：测试工具集位置

### 约束

未来 `cmd/sproxy/` 和 `cmd/sclient/` 可能各自独立为 Go module（独立 `go.mod`，不再属于 `github.com/cocomhub/sproxy` module）。

这意味着项目根目录的 `internal/` 包**不会被独立后的 cmd module 所见**（Go 的 `internal` 可见性规则限定在同一 module 树内）。

### 方案

将可复用测试工具集放在 **`pkg/testutil/`**（`github.com/cocomhub/sproxy/pkg/testutil`）：

| 对比项 | `internal/testutil/` | `pkg/testutil/` |
|--------|---------------------|-----------------|
| 当前 monomodule | ✅ 所有包可见 | ✅ 所有包可见 |
| cmd 独立为 module 后 | ❌ 不允许导入 | ✅ 可作为外部依赖导入（当 cmd 依赖根 module 时） |
| 未来最大化可移植 | ❌ 锁定在根 module | ✅ 可提取为独立 repo / workspace module |
| 被外部项目依赖 | ❌ 不可见 | ✅ 可能被依赖（但这是测试工具集，可接受） |

**结论**: `pkg/testutil/` 更优：
1. 当前 monomodule 下所有包（包括 `cmd/`）都能直接 `import "github.com/cocomhub/sproxy/pkg/testutil"`
2. 未来 cmd 独立为 module 后，如果它仍依赖根 module（如 `pkg/client/`），则 `pkg/testutil/` 仍可通过 `go.mod` 中的 `require` 导入
3. 即使彻底解耦，`pkg/testutil/` 也可被提取为独立 repo 或放入 Go workspace

### 提取清单

| 函数 | 签名 | 来源 |
|------|------|------|
| `TestKey()` | `func() string` | `server_test_common_test.go:testKey()` + `e2e_test.go:makeKey()` |
| `DiscardLogger()` | `func() *slog.Logger` | `server_test_common_test.go:testLogger()` |
| `SHA256Hex(data []byte)` | `func([]byte) string` | `integration_test.go`, `e2e_test.go` 中的 `sha256hex()` |
| `CaptureStdout(fn func())` | `func(func()) string` | `cmd/sclient/cmd_test.go:captureStdout()` |
| `CaptureStderr(fn func())` | `func(func()) string` | `cmd/sclient/cmd_test.go:captureStderr()` |

### 不移入 testutil 的函数

以下函数因高度耦合于特定包，**保留在原处**：

- `newTestServer` / `newTestServerWithAllRoutes` — 深度耦合 `pkg/server`
- `uploadFile` — 耦合 multipart 处理 + server 响应格式
- `makeReadOnlyDir` — Unix-only，平台特定
- `newMockServer` — 耦合 mock 端点定义
- `withHeader` — 仅 server 包内使用，不需跨包

## CLAUDE.md 记录指引

将以下内容记录到 sproxy 的 `CLAUDE.md` 中，以指导后续测试相关开发：

```
## 测试规范

### 测试工具集
跨包可复用的测试辅助函数位于 `pkg/testutil/`（避免放在 `internal/` 或 `cmd/` 下，
以兼顾未来 cmd 独立为 go module 时的可达性）。

### 测试约束
- 纯标准库测试：无 testify/gomock
- 127.0.0.1 回环绑定：所有 HTTP 测试服务必须监听 127.0.0.1，避免 Windows 防火墙弹窗
- Windows 兼容：路径用 filepath.Join/ToSlash，除 Unix-only 测试外必须在 Windows 通过
- 全局状态隔离：测试 cmd/sproxy 和 cmd/sclient 时须用 t.Cleanup 恢复包级全局变量
- Viper 隔离：测试优先使用 `viper.New()` 而非 `GetViper()` 全局单例

### 现有测试辅助函数查找
- `pkg/testutil/` — 跨包通用（TestKey, DiscardLogger, SHA256Hex, CaptureStdout, CaptureStderr）
- `pkg/server/server_test_common_test.go` — server 包内共享（testKey, testLogger, withHeader）
- `pkg/server/integration_test.go` — newTestServer + 变体
- `pkg/client/client_test.go` — newMockServer
- `test/e2e_test.go` — 端到端二进制测试 startSPROXY
```

## 测试规范（指导未来测试编写）

### 包级全局状态隔离

`cmd/sproxy` 和 `cmd/sclient` 使用包级全局变量（`cfgPtr`、`currentDir` 等），测试必须建立 save/restore 模式：

```go
// 每次测试结束恢复全局状态
func TestXxx(t *testing.T) {
    oldDir := currentDir
    t.Cleanup(func() { currentDir = oldDir })
    // ... test body
}
```

### Cobra 命令测试模式

```go
// 使用 rootCmd.SetArgs 注入参数，httptest.Server 模拟服务端
func TestUploadCommand(t *testing.T) {
    mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 验证请求格式，返回模拟响应
    }))
    defer mock.Close()

    rootCmd.SetArgs([]string{"upload", "--server", mock.URL, "testfile.txt"})
    defer rootCmd.SetArgs(nil)  // 重置

    err := rootCmd.Execute()
    // 验证 err 和输出
}
```

### Viper 隔离

测试中优先创建独立 Viper 实例，避免全局单例污染：

```go
v := viper.New()
v.Set("key", "value")
cfg := server.LoadFromViper(v)
// 而非 viper.GetViper().Set(...)
```

## 第一阶段：基础设施

### 1.1 `internal/testutil/testutil.go` — 包级测试工具集

包路径: `github.com/cocomhub/sproxy/internal/testutil`

实现上述提取清单中的 5 个函数。

### 1.2 全局状态隔离

- `cmd/sproxy/root_test.go` 中为 `cfgPtr`、`cfgFile`、`currentTunnelKeyHex` 添加 `t.Cleanup` 重置
- `cmd/sclient/cmd_test.go` 中为 `currentDir`、`cfgFile` 添加 `t.Cleanup` 恢复

## 第二阶段：覆盖缺口填补

### 2.1 `internal/shortid/shortid_test.go` （新建）

3 个测试用例覆盖全部 3 条分支。

### 2.2 `cmd/sclient/cmd_test.go` （追加，~12 个测试）

| 测试函数 | 模拟端点 | 验证 |
|----------|----------|------|
| `TestSearchCommand_HappyPath` | `GET /api/files/search?q=` → 200 | stdout 含文件名 |
| `TestSearchCommand_NoResults` | → 200 + 空列表 | stdout 无输出 |
| `TestMvCommand_HappyPath` | `POST /rename` → 200 | stdout 成功 |
| `TestMvCommand_ServerError` | → 400 | stderr 错误 |
| `TestStatCommand_HappyPath` | `HEAD /api/files/stat` → 200 | stdout checksum |
| `TestStatCommand_NotFound` | → 404 | stderr |
| `TestBatchDeleteCommand` | `POST /api/batch/delete` → 200 | stdout 计数 |
| `TestBatchRenameCommand` | `POST /api/batch/rename` → 200 | stdout |
| `TestArchiveCommand` | `POST /api/archive` → 200 | stdout |
| `TestGenkeyCommand` | 直接调用 | stdout hex key |
| `TestTunnelCommand` | `POST /tunnel` → 200 | stdout 响应体 |

### 2.3 `cmd/sclient/cd_test.go` （追加，5 个测试）

| 测试函数 | 输入组合 | 预期 |
|----------|----------|------|
| `TestResolveRemotePath_Absolute` | `/abs/path`, dir="" | `"abs/path"` |
| `TestResolveRemotePath_Root` | `/`, dir="" | `""` |
| `TestResolveRemotePath_DotDot` | `../escape`, dir="sub" | error |
| `TestResolveRemotePath_CurrentDirSet` | `file`, dir="sub" | `"sub/file"` |
| `TestResolveRemotePath_CurrentDirEmpty` | `file`, dir="" | `"file"` |

### 2.4 `cmd/sproxy/root_test.go` （追加，7 个测试）

| 测试函数 | 关键验证 |
|----------|---------|
| `TestResolveTunnelKey_EmptyAutoGenerate` | key 非空且 32 字节 |
| `TestResolveTunnelKey_SaveError` | 写失败时返回 error |
| `TestHandleSighup_KeyRotation` | `UpdateKey` 被调用 |
| `TestHandleSighup_ConfigReload` | `cfgPtr.Load()` 更新 |
| `TestHandleSighup_AuthTokenReload` | 新 token 生效 |
| `TestInitLogger_Combinations` | 8 组合均返回非 nil |
| `TestRunServer_ListenAndServeError` | 端口占用错误 |

### 2.5 `pkg/client/client_test.go` （追加）

| 测试函数 | 验证 |
|----------|------|
| `TestCalcChunkSize_EdgeCases` | fileSize=0 等边界 |
| `TestCalcChunkSize_SmallFile` | 不放大 |
| `TestCalcChunkSize_LargeFile` | 放大到上限 |
| `TestCalcChunkSize_Boundary` | 未触发放大 |
| `TestGenerateUploadID_Deterministic` | 相同输入相同输出 |
| `TestCloseBodyIfErr_ErrorWithNilResp` | resp=nil 时安全 |

### 2.6 `pkg/client/chunked_test.go` （新建）

| 测试函数 | 验证 |
|----------|------|
| `TestTryDownloadChunk_LengthMismatch` | false |
| `TestTryDownloadChunk_ChecksumMismatch` | false |
| `TestTryDownloadChunk_Non200` | false |

### 2.7 `pkg/server/handlers_test.go` （填充现有空文件）

`parsePagination` 表驱动测试：默认值、负 offset、零 limit、超限上限、正常值。

### 2.8 `pkg/server/version_test.go` （追加）

`deleteVersionHandler`：禁用返回 501、缺 filename 返回 400、正常删除返回 200。

### 2.9 `pkg/server/server_hub_test.go` （追加）

Hub 启用后的 `hubNodesHandler`、`hubRemoveNodeHandler`、`hubStatsHandler`。

### 2.10 `pkg/server/dirs_test.go` （新建）

`mkdir`/`rmdir`：正常、空参数、路径穿越。

### 2.11 `pkg/tunnel/tunnel_test.go` （追加）

| 测试函数 | 验证 |
|----------|------|
| `TestHandler_UpdateKey_OldKeyStillWorks` | 旧密钥可用（200） |
| `TestHandler_ServeHTTP_EmptyKey` | 403 |
| `TestDispatchLocal_PanicRecovery` | 500 + 不崩溃 |
| `TestForwardExternal_HTTPClientError` | 502 |

## 第三阶段：难以覆盖场景分析

| # | 场景 | 位置 | 根因 | 决策 |
|---|------|------|------|------|
| 1 | GzipMiddleware fallback | `gzip.go:49` | `gzip.NewWriterLevel(DefaultCompression)` 不会失败 | **跳过**，路径不可达 |
| 2 | `cfgPtr` 全局泄漏 | `root.go:30` | atomic.Pointer 跨测试共享 | **Save/Restore 模式** |
| 3 | `viper.GetViper()` 污染 | root.go both | 全局单例 | **测试用 `viper.New()`** |
| 4 | `os.Exit(1)` | sclient 多处 | 退出进程无法测试 | **RunE 返回 error，安全** |
| 5 | mTLS 配置 | `root.go` | 需真实 CA | **用 `tlsgen.go` 或 t.Skip** |
| 6 | `forwardExternal` 502 | tunnel.go | 需不可达 target | **已关闭 httptest.Server** |
| 7 | `dispatchLocal` panic | tunnel.go | 需 panic handler | **匿名 handler 触发** |

## CLAUDE.md 记录指引

将以下内容记录到 sproxy 的 `CLAUDE.md` 中，以指导后续测试相关开发：

```
## 测试规范

### 测试工具集
跨包可复用的测试辅助函数位于 `internal/testutil/`（避免放在 cmd/ 下，以免子包化后无法复用）。

### 测试约束
- 纯标准库测试：无 testify/gomock
- 127.0.0.1 回环绑定：所有 HTTP 测试服务必须监听 127.0.0.1，避免 Windows 防火墙弹窗
- Windows 兼容：路径用 filepath.Join/ToSlash，除 Unix-only 测试外必须在 Windows 通过
- 全局状态隔离：测试 cmd/sproxy 和 cmd/sclient 时须用 t.Cleanup 恢复包级全局变量
- Viper 隔离：测试优先使用 `viper.New()` 而非 `GetViper()` 全局单例

### 现有测试辅助函数查找
- `internal/testutil/` — 跨包通用（TestKey, DiscardLogger, SHA256Hex, CaptureStdout, CaptureStderr）
- `pkg/server/server_test_common_test.go` — server 包内共享（testKey, testLogger, withHeader）
- `pkg/server/integration_test.go` — newTestServer + 变体
- `pkg/client/client_test.go` — newMockServer
- `test/e2e_test.go` — 端到端二进制测试 startSPROXY
```

## 执行顺序

1. `internal/testutil/testutil.go` — 基础设施提取
2. `internal/shortid/shortid_test.go` — 最小改动
3. `pkg/server/handlers_test.go` — parsePagination
4. `pkg/server/dirs_test.go` — mkdir/rmdir
5. `pkg/server/version_test.go` — deleteVersion
6. `pkg/server/server_hub_test.go` — hub handlers
7. `cmd/sclient/cmd_test.go` + `cd_test.go` — 最大覆盖提升
8. `cmd/sproxy/root_test.go` — SIGHUP/密钥/日志
9. `pkg/client/client_test.go` + `chunked_test.go`
10. `pkg/tunnel/tunnel_test.go`

## 验证

```bash
# 各包覆盖率
go test -race -count=1 -coverprofile=out ./pkg/server/ && go tool cover -func=out | grep "total"
go test -race -count=1 -coverprofile=out ./pkg/client/ && go tool cover -func=out | grep "total"
go test -race -count=1 -coverprofile=out ./cmd/... && go tool cover -func=out | grep "total"
go test -race -count=1 -coverprofile=out ./internal/... && go tool cover -func=out | grep "total"
go test -race -count=1 -coverprofile=out ./pkg/tunnel/... && go tool cover -func=out | grep "total"

# 全量 race + 覆盖率
go test -race -shuffle=on -count=1 ./... 2>&1
go test -count=1 -coverprofile=cover.out ./... && go tool cover -func=cover.out | grep "total"
```
