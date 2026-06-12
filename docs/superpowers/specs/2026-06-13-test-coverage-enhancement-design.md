# 测试补全与覆盖率提升设计方案

## 概述

### 目标

针对 sproxy 项目当前 66.0% 的语句覆盖率，通过 4 个阶段的增量测试补全，将覆盖率提升至 **73-75%**，并实现以下改进：

1. **核心 0% 覆盖函数归零** — TunnelHandler、Handler、auth permissionAllowed、metrics.Snapshot、UpdateKey、hub 管理 handler、client 配置/归档/版本管理
2. **分块上传/下载深层分支覆盖** — 续传恢复、重试耗尽、checksum 不匹配、边界条件
3. **CLI 入口测试覆盖** — cobra 命令 `RunE` 和端到端二进制测试
4. **消除测试代码重复** — 提取 sha256hex、统一 test server 工厂函数签名

### 约束

- **纯标准库**：不使用 testify、gomega 等第三方断言库，延续现有 `t.Fatalf`/`t.Errorf` 模式
- **127.0.0.1 回环绑定**：所有测试服务端监听 `127.0.0.1`（`httptest.NewServer` 默认行为），避免 Windows 防火墙弹窗
- **最小改动**：不重构现有测试，只在现有文件新增测试或创建新测试文件
- **`_test.go` 同包**：延续现有 `package server`（内部包测试）/ `package server_test`（外部包测试）混合模式

---

## Phase 1：Server 核心 0% 覆盖补全

### 1.1 auth 鉴权 (server_auth_test.go)

#### permissionAllowed

```go
func TestPermissionAllowed(t *testing.T) {
    t.Parallel()
    tests := []struct {
        name string
        cfg  Config  // 含 AuthToken 和 APIKeys 配置
        r    *http.Request  // 含 Authorization header
        want bool
    }{
        {name: "no auth configured → allow all",
         cfg: Config{},  // AuthToken="" APIKeys=nil
         r:   httptest.NewRequest("GET", "/", nil),
         want: true},
        {name: "auth token match → allowed",
         cfg: Config{AuthToken: "secret"},
         r:   withHeader(httptest.NewRequest(...), "Authorization", "Bearer secret"),
         want: true},
        {name: "auth token mismatch → denied",
         cfg: Config{AuthToken: "secret"},
         r:   withHeader(httptest.NewRequest(...), "Authorization", "Bearer wrong"),
         want: false},
        // APIKeys 匹配/失配/缺失 Authorization header 等 case
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            h := &Handlers{cfg: &tt.cfg}
            got := h.permissionAllowed(tt.r)
            if got != tt.want { t.Errorf(...) }
        })
    }
}
```

**预期覆盖**：`permissionAllowed` 从 0% → 100%

#### authMiddleware 补充分支

现有 `authMiddleware` 测试在 `integration_test.go` 中，但缺少 401 未授权场景的精确断言。

```go
func TestAuthMiddleware_NoToken(t *testing.T) {
    h := Handlers{cfg: &Config{AuthToken: "required"}}
    handler := h.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        t.Error("should not reach inner handler")
    }))
    r := httptest.NewRequest("GET", "/upload", nil)
    w := httptest.NewRecorder()
    handler.ServeHTTP(w, r)
    if w.Code != http.StatusUnauthorized { ... }
    // 验证响应 JSON 含 Success=false
}
```

### 1.2 Handler 集成入口 (server_handler_gaps_test.go)

#### TunnelHandler / Handler

```go
func TestTunnelHandler_ReturnsHandler(t *testing.T) {
    h := Handlers{cfg: &Config{TunnelKey: key64hex}}
    th := h.TunnelHandler()
    if th == nil { t.Fatal("TunnelHandler returned nil") }
}

func TestHandler_RegisterRoutes(t *testing.T) {
    h := NewHandlers(cfg, logger)
    srv := httptest.NewServer(h.Handler())
    defer srv.Close()
    // 验证公开路由可用
    resp, _ := http.Get(srv.URL + "/healthz")
    if resp.StatusCode != 200 { ... }
    // 验证 /upload 需要 auth
    resp, _ = http.Get(srv.URL + "/upload")
    if resp.StatusCode != 401 { ... }
}
```

#### UpdateKey

```go
func TestUpdateKey(t *testing.T) {
    key1, _ := tunnel.ParseKey(genKey())
    key2, _ := tunnel.ParseKey(genKey())
    h := NewLocalHandler(key1, nil, logger)
    // 用 key1 能连接
    // 调 UpdateKey(key2)
    // 用 key1 连不上（403）
    // 用 key2 能连接
}
```

### 1.3 Metrics Snapshot (server_metrics_test.go 补充)

```go
func TestMetricsSnapshot(t *testing.T) {
    m := NewMetrics("sproxy")
    m.RecordRequest(200, "upload", "1s")
    m.RecordBytes(1024, "download")
    s := m.Snapshot()
    if s.RequestsTotal != 1 { ... }
    if s.BytesSent != 0 || s.BytesReceived != 1024 { ... }
}
```

### 1.4 Hub 管理 handler (server_hub_test.go)

```go
func TestHubNodesHandler(t *testing.T) {
    // 构造带 routeTable 的 Handlers，注册一个虚拟节点
    // httptest.NewRequest("GET", "/api/hub/nodes", nil)
    // 验证返回 JSON 含节点列表
}
func TestHubStatsHandler_NoHub(t *testing.T) {
    // hub.Enabled=false → 返回 400
}
```

---

## Phase 2：Client 未覆盖业务逻辑

### 2.1 客户端配置 (client_config_test.go 补充)

```go
func TestClientConfigValidate(t *testing.T) {
    tests := []struct {
        name string
        cfg  *Config
        want error  // nil / ErrMissingServerURL
    }{
        {name: "valid config", cfg: &Config{ServerURL: "http://localhost:8080", Timeout: 30}},
        {name: "missing url",   cfg: &Config{Timeout: 30}, want: ErrMissingServerURL},
        {name: "negative timeout", cfg: &Config{ServerURL: "...", Timeout: -1}, want: ...},
    }
}
```

```go
func TestClientConfigLoadFromViper(t *testing.T) {
    v := viper.New()
    v.Set("server_url", "http://test:8080")
    cfg := LoadFromViper(v)
    if cfg.ServerURL != "http://test:8080" { ... }
    // 测试默认值 fallback
}
```

```go
func TestHandleConfigShow(t *testing.T) {
    // captureStdout 捕获输出
    // 验证输出含 "server_url"、"timeout" 等 key
}
```

### 2.2 Client Archive (client_archive_test.go)

依赖 mock server，需要构造 tar 响应：

```go
func TestClientArchive(t *testing.T) {
    mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        tw := tar.NewWriter(w)
        tw.WriteHeader(&tar.Header{Name: "a.txt", Size: 4})
        tw.Write([]byte("data"))
        tw.Close()
    }))
    defer mock.Close()
    c := NewFileClient(mock.URL)
    dst := t.TempDir() + "/out.tar"
    err := c.Archive(context.Background(), []string{"a.txt"}, dst)
    // 验证 dst 文件存在且内容匹配
}
```

同样覆盖：`ArchiveDir`（目录打包）、空文件列表、下载失败时清理。

### 2.3 Client Version (client_version_test.go)

```go
func TestClientListVersions(t *testing.T) {
    mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        json.NewEncoder(w).Encode([]VersionInfo{{ID: "v1", File: "a.txt", ...}})
    }))
    c := NewFileClient(mock.URL)
    versions, err := c.ListVersions(context.Background(), "a.txt")
    // 验证返回 []VersionInfo
}
```

覆盖：list / restore / delete 各 200、400、404 路径。

---

## Phase 3：分块操作深层分支

### 3.1 分块上传补充 (server_chunked_upload_test.go 补充)

**关键新增场景：**

| 场景 | mock 实现 | 断言 |
|---|---|---|
| **续传恢复** | `initSession` 后只上传部分 chunk；`uploadStatus` 返回已接收列表；调用 `uploadChunk` 补传缺失 chunk | 验证 `uploadComplete` 成功，只上传了缺失的 chunk |
| **重试耗尽** | mock handler 在第 N 个 chunk 恒返回 500；client 端重试 3 次后报错 | client 返回 `ErrUploadFailed`，server 端未合并文件 |
| **并发冲突** | `uploadChunk` 返回 409（文件已存在但 checksum 不同） | client 正确处理冲突 |
| **超时取消** | `context.WithTimeout(ctx, 1ms)` + chunk handler sleep 100ms | client 返回 `context.DeadlineExceeded` |
| **所有 chunk 已上传重新 complete** | 文件已完整上传 → `uploadComplete` 幂等返回 200 | 文件最终存在且 checksum 一致 |

**续传测试关键代码示意：**
```go
func TestChunkedUpload_Resume(t *testing.T) {
    // 1. init session → 得到 upload_id
    // 2. 上传 chunk 0, 1, 2（共 6 个）
    // 3. uploadStatus → 返回已接收 [0,1,2]
    // 4. 客户端只补传 chunk 3,4,5
    // 5. uploadComplete → 成功
    // 6. 验证最终文件 checksum 一致
}
```

### 3.2 分块下载补充 (server_chunked_download_test.go 补充)

| 场景 | mock 实现 | 断言 |
|---|---|---|
| **单 chunk 失败后恢复** | 请求 chunk 0 时返回 500，chunk 1+ 返回正常 → client 重试 chunk 0 | 最终下载完成 |
| **Checksum 不匹配** | mock 返回错误的 `X-Chunk-Checksum` header | client 报 `ErrChecksumMismatch` |
| **416 越界** | 请求 offset >= file size | 返回 416 |
| **空文件** | content-length 为 0 的文件 | 下载成功，文件为空，checksum 正确 |
| **并发下载部分失败** | 4 goroutine 中 1 个失败后恢复 | 最终合并文件正确 |

### 3.3 消除重复 test helper

在 `pkg/server/` 中新建 `server_test_common.go`（`_test.go` 后缀，包内可见）：

```go
// server_test_common.go — 注意文件名为 _test 但内容为共用 helper
func sha256hex(b []byte) string {
    return hex.EncodeToString(sha256.Sum256(b)[:])
}

// 可测试用 key 生成
func testKey(t testing.TB) string {
    t.Helper()
    return strings.Repeat("a", 64)
}
```

同时在 `pkg/server/e2e_test.go`（外部包 `server_test`）中通过复制引用或统一到内部包测试来消重。如果外部包无法引用内部 helper，可保留副本但加注释说明重复来源。

---

## Phase 4：CLI 集成 + 端到端

### 4.1 sproxy CLI (cmd/sproxy/root_test.go 补充)

```go
func TestRunServer_StartStop(t *testing.T) {
    // 准备临时目录
    tmpDir := t.TempDir()
    // 配置启动参数：127.0.0.1:0（随机端口）
    cfg := &server.Config{
        Addr: "127.0.0.1:0",
        UploadsDir: tmpDir,
    }
    // 启动 server goroutine
    ctx, cancel := context.WithCancel(context.Background())
    errCh := make(chan error, 1)
    go func() { errCh <- runServer(ctx, cfg, logger) }()
    t.Cleanup(cancel)
    // 等待服务就绪（轮询 /healthz）
    // 验证 /version 返回版本信息
    // cancel → 验证优雅关闭
}
```

### 4.2 sclient CLI (cmd/sclient/cmd_test.go 补充)

通过构造 cobra 命令、捕获 stdout/stderr 来测：

```go
func TestUploadCommand(t *testing.T) {
    mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // 验证请求含 multipart 表单和 X-File-Checksum header
        // 返回成功 JSON 响应
    }))
    defer mock.Close()
    // 设置 rootCmd 的 server_url flag
    rootCmd.SetArgs([]string{"upload", "--server", mock.URL, "testdata/sample.txt"})
    // 调 Execute()
    // 验证 stderr 无错误输出
}
```

覆盖 upload/download/delete/list 四个核心子命令的 happy path + 错误路径。

### 4.3 端到端二进制测试 (test/e2e_binary_test.go)

```go
func TestMain(m *testing.M) {
    // build 二进制到临时目录
    // 启动 sproxy 进程（127.0.0.1:随机端口）
    // 设置环境变量 SCLIENT_SERVER_URL
    code := m.Run()
    // 清理
    os.Exit(code)
}
func TestE2E_UploadDownloadDelete(t *testing.T) {
    // 创建临时文件
    // sclient upload → 验证成功
    // sclient list → 验证文件在列表中
    // sclient download → 验证内容一致
    // sclient delete → 验证删除成功
}
```

**约束**：仅在 `go test -tags=e2e` 时运行，不影响常规单元测试。

---

## 测试文件清单

| 阶段 | 文件路径 | 操作 | 新增/补充 |
|---|---|---|---|
| 1 | `pkg/server/server_auth_test.go` | **新建** | ~100 行 |
| 1 | `pkg/server/server_handler_gaps_test.go` | **新建** | ~120 行 |
| 1 | `pkg/server/server_metrics_test.go` | 补充现有 | +~40 行 |
| 1 | `pkg/server/server_hub_test.go` | **新建** | ~80 行 |
| 2 | `pkg/client/client_config_test.go` | 补充现有 | +~80 行 |
| 2 | `pkg/client/client_archive_test.go` | **新建** | ~100 行 |
| 2 | `pkg/client/client_version_test.go` | **新建** | ~100 行 |
| 3 | `pkg/server/server_chunked_upload_test.go` | 补充现有 | +~120 行 |
| 3 | `pkg/server/server_chunked_download_test.go` | 补充现有 | +~80 行 |
| 3 | `pkg/server/server_test_common.go` | **新建** | ~20 行 |
| 4 | `cmd/sproxy/root_test.go` | 补充现有 | +~80 行 |
| 4 | `cmd/sclient/cmd_test.go` | 补充现有 | +~120 行 |
| 4 | `test/e2e_binary_test.go` | **新建** | ~150 行 |

**总计新增约 1200 行测试代码，覆盖率预估 66% → 73-75%。**

---

## 验证方法

每个阶段完成后执行：

```bash
# 验证编译
go build ./...

# 验证测试通过 + race 检测
go test -race -count=1 ./pkg/server/...
go test -race -count=1 ./pkg/client/...
go test -race -count=1 ./cmd/...

# 验证覆盖率
go test -coverprofile=cover.out ./...
go tool cover -func=cover.out | grep "total"

# 确认 0% 函数减少
go tool cover -func=cover.out | grep "0.0%" | wc -l
```

最终验证端到端（需要 `-tags=e2e`）：

```bash
go test -tags=e2e -count=1 ./test/...
```
