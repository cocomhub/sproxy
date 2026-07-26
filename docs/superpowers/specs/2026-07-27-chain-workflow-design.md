# 链式工作流 API 设计规格

## 1. 概述

### 1.1 目标

sproxy 需要支持被外部库灵活使用，实现"本地服务 ↔ 公网服务器"的完整工作流：
1. 本地通过公网服务器发起云端离线下载（URL → 文件）
2. 等待下载完成后，在服务端打包为 tar.gz 归档
3. 下载归档到本地
4. 自动清理远端文件以释放空间
5. 所有场景保留原始文件 mtime（修改时间）

### 1.2 使用场景

- **中规模日常使用**：每次几十个文件，总大小在 GB 级别
- **SDK + CLI 混合模式**：核心逻辑用 Go SDK 编写，CLI 提供调试入口
- **传输加密（服务器可读明文）**：AES-256-GCM 隧道或 HTTPS，服务器可读文件内容
- **数据完整性与断点续传并重**

---

## 2. 架构设计

### 2.1 组件关系

```
┌─────────────────────────────────────────────────────────┐
│                    FileClient                             │
├─────────────────────────────────────────────────────────┤
│  CloudDownloadChain()  ← 一键链式入口                   │
│  ResumeChain()          ← 断点恢复                       │
│  ListChains() / DeleteChain()  ← 管理                   │
├─────────────────────────────────────────────────────────┤
│  ChainManager  ← 通用编排层                              │
│    ├── Run(ctx, runner)  ← 执行 + 自动持久化             │
│    ├── Resume(ctx, id)   ← 从断点恢复                    │
│    └── List() / Delete()  ← 管理                        │
├─────────────────────────────────────────────────────────┤
│  ChainRunner 接口  ← 链式操作执行器                       │
│    ├── CloudDownloadChain  ← 云下载链式操作（本次实现）    │
│    └── FutureChain         ← 未来扩展                    │
├─────────────────────────────────────────────────────────┤
│  KVStore 接口  ← 通用键值存储                            │
│    ├── JSONKVStore (默认)  ← JSON 文件                    │
│    └── BoltKVStore (未来)  ← bbolt 数据库                 │
├─────────────────────────────────────────────────────────┤
│  Registry[KVStore]  ← 插件注册表                         │
└─────────────────────────────────────────────────────────┘
```

### 2.2 数据流

```
Client (本地)                         Server (公网)
    │                                      │
    │ 1. CloudDownloadChain(urls, ...)      │
    │  → ChainManager.Run(CloudDownloadChain) │
    │  → 自动持久化初始状态                   │
    │─────────────────────────────────────►│
    │ 2. POST /api/cloud/download/batch    │
    │  (提交 N 个 URL, 创建 N 个任务)       │
    │◄─────────────────────────────────────│
    │ return [{id: "cloud-xxx", status: "pending"}]  │
    │                                      │
    │ 3. GET /api/cloud/tasks 轮询 (3s)    │
    │  ──[循环]───────────────────────────►│
    │  │  [HTTPDownloader 从 Last-Modified │
    │  │   提取原始文件 mtime               │
    │  │  CloudTask.FileMTime 保存 mtime]  │
    │  │  Server: downloading → completed  │
    │  │  若 507 → 标记待重试, 30s 后重试   │
    │  ────────────────────────────────────│
    │                                      │
    │ 4. POST /api/cloud/archive (打包)    │
    │  (tar.gz 保留原始文件 ModTime)        │
    │  │  [归档后自动删除 __cloud__/ 原始文件]│
    │◄─────────────────────────────────────│
    │ return {file, checksum, size}        │
    │                                      │
    │ 5. GET /download/chunk (分块下载)    │
    │  (断点续传 + 指数退避 + checksum)     │
    │  │  [X-File-MTime 恢复本地 mtime]     │
    │◄─────────────────────────────────────│
    │                                      │
    │ 6. 默认清理远端                       │
    │  DELETE /api/cloud/tasks/{id}        │
    │  DELETE 归档文件                      │
    │                                      │
    │ 7. 持久化 ChainState → KVStore       │
    │  (缓存文件, 支持重启恢复)             │
    │                                      │
    │ 8. 返回 ChainResult                  │
```

---

## 3. 接口设计

### 3.1 KVStore 通用键值存储接口

```go
// pkg/client/store.go

// KVStore 通用键值存储接口。
// 所有方法接收 ctx context.Context 以支持 trace 传播。
type KVStore interface {
    // Save 保存 key 对应的数据（原子写入语义）
    Save(ctx context.Context, key string, value map[string]any) error
    // Load 加载 key 对应的数据
    Load(ctx context.Context, key string) (map[string]any, error)
    // List 列出指定前缀的所有 key
    List(ctx context.Context, prefix string) ([]string, error)
    // Delete 删除 key
    Delete(ctx context.Context, key string) error
    // Close 关闭存储，释放资源
    Close() error
}
```

### 3.2 StructCodec 结构体编解码

```go
// pkg/client/store.go

// StructCodec 在 struct（带 json tag）和 map[string]any 之间转换。
// 使用 json.Marshal → json.Unmarshal 到 map 实现，保证字段名一致性。
type StructCodec struct{}

func (StructCodec) ToMap(v any) (map[string]any, error)
func (StructCodec) FromMap(m map[string]any, v any) error
```

### 3.3 插件注册表

```go
// pkg/client/store.go

// KVStoreRegistry 是可插拔的 KVStore 注册表。
// 默认使用 JSONKVStore（方案 A），可通过独立 go.mod 注册其他实现。
var KVStoreRegistry = plugin.New[KVStore]("kv_store", &jsonKVStoreBuiltin{})

// KVStoreFactory KVStore 工厂接口
type KVStoreFactory interface {
    Name() string
    Open(ctx context.Context, cfg map[string]string) (KVStore, error)
}
```

### 3.4 JSONKVStore 默认实现

```go
// pkg/client/store_json.go

// JSONKVStore 基于 JSON 文件的 KVStore 实现。
// 每个 key 对应一个 .json 文件，原子写入（tmp → rename）。
type JSONKVStore struct {
    dir    string
    logger *slog.Logger
}

func NewJSONKVStore(ctx context.Context, dir string, logger *slog.Logger) (*JSONKVStore, error)
// Save: json.Marshal → WriteFile(path.tmp) → Rename(path)
// Load: ReadFile → json.Unmarshal → map[string]any
// List: ReadDir → 过滤 .json 后缀 → 匹配前缀
// Delete: Remove file
```

### 3.5 ChainRunner 接口（通用链式执行器）

```go
// pkg/client/chain.go

// ChainRunner 链式操作执行器接口。
// 每个链式操作实现此接口，ChainManager 负责编排、持久化和恢复。
type ChainRunner interface {
    // ID 返回链式操作唯一标识
    ID() string
    // Phase 返回当前阶段名称（用于进度展示和持久化）
    Phase() string
    // Status 返回当前状态（running / completed / failed）
    Status() string
    // Run 执行链式操作
    // reportFn 用于阶段变更通知（ctx 携带 trace 信息）
    Run(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error
    // State 返回当前状态（用于持久化，map[string]any 格式）
    State() map[string]any
    // Restore 从持久化状态恢复
    Restore(state map[string]any) error
}
```

### 3.6 ChainManager 通用编排层

```go
// pkg/client/chain.go

// ChainManager 链式操作管理器，负责编排、持久化和恢复。
type ChainManager struct {
    store KVStore
    codec StructCodec
}

func NewChainManager(store KVStore) *ChainManager

// Run 执行链式操作（自动持久化，支持恢复）
// 1. 持久化初始状态  2. 执行 (reportFn 触发阶段变更持久化)  3. 完成/失败后更新状态
func (m *ChainManager) Run(ctx context.Context, runner ChainRunner) error

// Resume 从断点恢复链式操作
// 1. 从 KVStore 加载状态  2. 判断 runner 类型  3. Restore  4. Run
func (m *ChainManager) Resume(ctx context.Context, chainID string) (ChainRunner, error)

// List 列出所有活跃链式操作
func (m *ChainManager) List(ctx context.Context) ([]ChainRunner, error)

// Delete 删除链式操作缓存
func (m *ChainManager) Delete(ctx context.Context, chainID string) error

// 内部: saveState / loadState / resolveRunner（根据 type 字段决定 runner 类型）
```

### 3.7 CloudDownloadChain 实现

```go
// pkg/client/chain_cloud_download.go

// CloudDownloadChain 云端下载链式操作，实现 ChainRunner 接口。
type CloudDownloadChain struct {
    // 标识
    chainID string
    phase   string
    status  string

    // 业务字段
    URLs        []string
    TaskIDs     []string
    ArchiveName string
    LocalDir    string
    LocalPath   string
    KeepFiles   bool

    // 依赖
    client *FileClient
    opts   chainOptions

    // 统计
    Completed int
    Failed    int
    Total     int
    Error     string
    CreatedAt time.Time
    UpdatedAt time.Time
}

// ChainRunner 接口实现
func (c *CloudDownloadChain) ID() string
func (c *CloudDownloadChain) Phase() string
func (c *CloudDownloadChain) Status() string

// State 返回持久化状态（含 type 字段用于恢复时判断）
func (c *CloudDownloadChain) State() map[string]any

// Restore 从持久化状态恢复
func (c *CloudDownloadChain) Restore(state map[string]any) error

// Run 执行链式操作（从 phase 断点继续，跳过已完成阶段）
func (c *CloudDownloadChain) Run(ctx context.Context,
    reportFn func(ctx, phase, msg string, current, total int)) error {
    switch c.phase {
    case "": fallthrough
    case "submitting":
        // 1. 提交云端下载任务
        reportFn(ctx, "submitting", "提交云端下载任务", 0, len(c.URLs))
        ...
        c.phase = "waiting"
        c.Total = len(c.URLs)
    case "waiting": fallthrough
    case "waiting":
        // 2. 轮询等待完成（含存储超限重试）
        reportFn(ctx, "waiting", "等待下载完成", c.Completed, c.Total)
        ...
        c.phase = "archiving"
    case "archiving": fallthrough
    case "archiving":
        // 3. 服务端打包
        reportFn(ctx, "archiving", "打包归档", 0, 1)
        ...
        c.phase = "downloading"
    case "downloading": fallthrough
    case "downloading":
        // 4. 分块下载到本地（断点续传 + 指数退避 + checksum 验证）
        reportFn(ctx, "downloading", "下载到本地", 0, 1)
        ...
        c.phase = "cleaning"
    case "cleaning": fallthrough
    case "cleaning":
        // 5. 清理远端（默认删除，unless KeepFiles）
        if !c.KeepFiles {
            reportFn(ctx, "cleaning", "清理远端文件", 0, len(c.TaskIDs)+1)
            ...
        }
        c.phase = "completed"
        c.status = "completed"
    }
    return nil
}
```

### 3.8 FileClient 新增方法

```go
// CloudDownloadChain 一键链式操作
// 流程: 提交任务 → 等待完成 → 打包 → 下载到本地 → 清理远端
// 默认: 成功后自动删除远端所有相关文件
// 选项: WithChainKeepFiles() 保留远端文件
func (c *FileClient) CloudDownloadChain(ctx context.Context,
    urls []string, archiveName, localDir string,
    opts ...ChainOption) (*ChainResult, error)

// ResumeChain 从缓存恢复链式操作
func (c *FileClient) ResumeChain(ctx context.Context, chainID string) (*ChainResult, error)

// ListChains 列出所有活跃链式操作
func (c *FileClient) ListChains(ctx context.Context) ([]*ChainState, error)

// DeleteChain 删除链式操作缓存
func (c *FileClient) DeleteChain(ctx context.Context, chainID string) error
```

### 3.9 新增 Option

```go
// WithKVStore 设置自定义 KVStore 实现
func WithKVStore(store KVStore) Option

// WithCacheDir 使用默认 JSONKVStore 并指定缓存目录
func WithCacheDir(dir string) Option

// RunChain 通用链式操作入口（传 ChainRunner 实现）
func (c *FileClient) RunChain(ctx context.Context, runner ChainRunner) (*ChainResult, error)

// 链式操作选项
func WithChainKeepFiles() ChainOption
func WithChainPollInterval(d time.Duration) ChainOption
func WithChainTimeout(d time.Duration) ChainOption
func WithChainProgress(fn func(ctx context.Context, phase string, msg string, current, total int)) ChainOption
```

---

## 4. mtime 全链路保留

### 4.1 现有能力

| 场景 | 状态 | 说明 |
|------|------|------|
| 直接上传 | ✅ | `X-File-MTime` → 服务端 `os.Chtimes` |
| 直接下载 | ✅ | 服务端 `X-File-MTime` → 客户端 `os.Chtimes` |
| 分块上传 | ✅ | `file_mod_time` 字段 → 合并后 `os.Chtimes` |
| 分块下载 | ✅ | `restoreDownloadModTime` 已实现 |
| 归档 tar.gz | ⚠️ | `tar.FileInfoHeader` 自动保留 ModTime，但无测试验证 |

### 4.2 修复项

| 修复 | 文件 | 改动 |
|------|------|------|
| 下载器保留 mtime | `pkg/server/downloader/downloader.go` | `Result` 增加 `ModTime time.Time` |
| | `pkg/server/downloader/http_downloader.go` | 从 `Last-Modified` 响应头提取 mtime |
| CloudTask 保存 mtime | `pkg/server/cloud_download.go` | `CloudTask.FileMTime` 字段；下载后 `os.Chtimes` |
| 下载 Handler 返回 mtime | `pkg/server/download_handler.go` | 云文件路径正确设置 `X-File-MTime` |
| 归档测试 | `pkg/server/archive_test.go` | 验证 tar header ModTime == 原始文件 mtime |

### 4.3 数据流

```
URL 服务器 Last-Modified
  → HTTPDownloader 提取 modTime (http_downloader.go)
  → CloudTask.FileMTime 保存 (cloud_download.go)
  → os.Chtimes(destPath, modTime) (cloud_download.go)
  → tar.Header.ModTime 自动保留 (archive.go via FileInfoHeader)
  → X-File-MTime 响应头 (download_handler.go)
  → 客户端 os.Chtimes(outputPath, modTime) (client.go / chunked.go)
```

---

## 5. 弱网可靠性增强

### 5.1 分块下载指数退避

```go
// 在 chunked.go 中
func downloadOneChunk(ctx context.Context, ...) ([]byte, bool) {
    baseDelay := 500 * time.Millisecond
    for attempt := 0; attempt < maxRetries; attempt++ {
        data, ok := tryDownloadChunk(ctx, ...)
        if ok { return data, true }
        if attempt < maxRetries - 1 {
            delay := baseDelay * (1 << attempt) // 500ms, 1s, 2s
            select {
            case <-time.After(delay):
            case <-ctx.Done():
                return nil, false
            }
        }
    }
    return nil, false
}
```

### 5.2 存储超限客户端重试

```go
// 在 chain_cloud_download.go 中
func (c *CloudDownloadChain) waitForTasks(ctx, taskIDs, pollInterval, timeout, maxRetries) {
    for attempt := 0; attempt <= maxRetries; attempt++ {
        results := c.pollTasks(ctx, taskIDs, pollInterval, timeout)
        failedByStorage := filterStorageFull(results)
        if len(failedByStorage) == 0 { return results, nil }
        if attempt < maxRetries {
            select {
            case <-time.After(30 * time.Second):
            case <-ctx.Done():
                return results, ctx.Err()
            }
            for _, t := range failedByStorage {
                c.client.CloudDownload(ctx, t.URL)
            }
        }
    }
    return results, fmt.Errorf("storage full after %d retries", maxRetries)
}
```

---

## 6. 清理语义

- **默认行为**：`CloudDownloadChain` 本地下载成功后，自动删除远端所有文件
  - 归档文件（`__cloud_archives__/` 下）
  - 云任务原始文件（`__cloud__/<taskID>/` 下，调用 `DeleteTask`）
  - 释放 StorageManager 配额
- **保留文件**：通过 `WithChainKeepFiles()` 选项，保留远端文件
- **清理时机**：本地下载校验通过后（checksum 验证 + mtime 恢复）

---

## 7. 测试计划

### 7.1 新增测试文件

| 文件 | 测试 | 覆盖 |
|------|------|------|
| `pkg/client/store_test.go` | `TestJSONKVStore_SaveLoad` | 基本读写 |
| | `TestJSONKVStore_List` | 前缀列表 |
| | `TestJSONKVStore_Delete` | 删除 |
| | `TestJSONKVStore_AtomicWrite` | 原子写入（tmp→rename） |
| | `TestStructCodec_ToFromMap` | 结构体编解码 |
| `pkg/client/chain_test.go` | `TestChainManager_Run` | 基本编排 |
| | `TestChainManager_Resume` | 断点恢复 |
| | `TestChainManager_List` | 列表 |
| | `TestCloudDownloadChain` | 完整链式流程 |
| | `TestCloudDownloadChain_KeepFiles` | 保留远端文件 |
| | `TestCloudDownloadChain_StorageFullRetry` | 存储超限重试 |
| | `TestCloudDownloadChain_Timeout` | 超时处理 |
| `pkg/client/chunked_test.go` | `TestDownloadChunk_ExponentialBackoff` | 指数退避 |
| | `TestDownloadChunk_RetryThenSuccess` | 重试后成功 |

### 7.2 新增测试到现有文件

| 文件 | 新增测试 |
|------|---------|
| `pkg/server/archive_test.go` | `TestArchive_PreservesMTime` |
| `pkg/server/cloud_archive_handler_test.go` | `TestCloudArchive_PreservesMTime` |
| `pkg/server/cloud_download_test.go` | `TestCloudDownload_PreservesMTime` |
| | `TestCloudDownload_DeleteTaskCleansAndReleases` |
| `pkg/server/downloader/http_downloader_test.go` | `TestHTTPDownloader_PreservesMTime` |
| `test/e2e_test.go` | `TestE2E_CloudDownloadChain` |

---

## 8. 文件清单

### 8.1 新增文件

| 路径 | 内容 |
|------|------|
| `pkg/client/store.go` | `KVStore` 接口、`StructCodec`、`KVStoreRegistry`、`KVStoreFactory` |
| `pkg/client/store_json.go` | `JSONKVStore` 默认实现 |
| `pkg/client/chain.go` | `ChainRunner` 接口、`ChainManager`、`ChainOption`、`ChainResult` |
| `pkg/client/chain_cloud_download.go` | `CloudDownloadChain` 实现 |
| `pkg/client/chain_test.go` | 链式操作测试 |
| `pkg/client/store_test.go` | KVStore 测试 |

### 8.2 修改文件

| 路径 | 改动 |
|------|------|
| `pkg/client/client.go` | `FileClient` 新增 `chainManager` 字段、`WithKVStore`/`WithCacheDir` |
| `pkg/client/cloud.go` | 新增 `CloudDownloadBatch` 和 `ArchiveCloudTasks` 的 SDK 封装 |
| `pkg/client/chunked.go` | 指数退避重试 |
| `pkg/server/downloader/downloader.go` | `Result.ModTime` 字段 |
| `pkg/server/downloader/http_downloader.go` | 从 `Last-Modified` 提取 mtime |
| `pkg/server/cloud_download.go` | `CloudTask.FileMTime` 字段、下载后 `os.Chtimes` |
| `pkg/server/cloud_archive_handler.go` | 归档后自动清理选项 |
| `pkg/server/download_handler.go` | 云文件 mtime 正确传递 |
| `pkg/server/archive_test.go` | 新增 mtime 验证测试 |
| `pkg/server/cloud_archive_handler_test.go` | 新增 mtime 验证测试 |
| `pkg/server/cloud_download_test.go` | 新增 mtime + 清理测试 |
| `pkg/server/downloader/http_downloader_test.go` | 新增 mtime 测试 |
| `test/e2e_test.go` | 端到端链式流程测试 |

---

## 9. 未来扩展

### 9.1 其他 KVStore 实现插件

```go
// 独立 go.mod 包: github.com/cocomhub/sproxy-kvstore-bbolt
package bboltstore

import "github.com/cocomhub/sproxy/pkg/client"

func init() {
    client.KVStoreRegistry.Register(plugin.Plugin[client.KVStore]{
        Name:     "bbolt",
        Instance: &boltFactory{},
        Priority: 10,
    })
}
```

用户使用：

```go
import _ "github.com/cocomhub/sproxy-kvstore-bbolt"

client := NewFileClient(serverURL,
    WithKVStore("bbolt", map[string]string{"path": "./cache/sproxy.db"}),
)
```

### 9.2 其他链式操作

任何新链式操作只需实现 `ChainRunner` 接口：

```go
// 示例：上传 → 处理 → 下载链式操作
type UploadProcessChain struct {
    LocalPath   string
    ProcessCmd  string
    OutputName  string
    // ... 链式字段
}

func (c *UploadProcessChain) ID() string { ... }
func (c *UploadProcessChain) Phase() string { ... }
func (c *UploadProcessChain) Status() string { ... }
func (c *UploadProcessChain) Run(ctx, reportFn) error {
    // 上传 → 等待处理 → 下载 → 清理
}
func (c *UploadProcessChain) State() map[string]any { ... }
func (c *UploadProcessChain) Restore(state map[string]any) error { ... }

// 使用
result, err := client.RunChain(ctx, &UploadProcessChain{...})
```

---

## 10. 未涉及范围

- 服务端 `Server-Sent Events` 或 WebSocket 推送进度（当前轮询方案足够）
- 端到端加密（服务器可读明文，传输加密已由 AES-256-GCM / HTTPS 保障）
- Web UI 的链式操作入口（先满足 SDK 和 CLI 需求）