# Cloud Download → Archive → Download 全链路工作流设计

- **日期：** 2026-07-26
- **状态：** 设计稿
- **作者：** Claude Code

## 1. 背景与目标

### 1.1 目标场景

用户希望 sproxy 支持以下端到端工作流：

```
本地 sclient (Go SDK 或 CLI)
  → Tunnel 加密连接 → 公网 sproxy
  → 创建云端下载任务 (POST /api/cloud/download)
  → 监控任务完成
  → 打包归档 (POST /api/archive)
  → 下载到本地 (GET /download)
  → 本地解压 tar.gz
```

### 1.2 当前缺口

| 组件 | 状态 | 详情 |
|------|------|------|
| `pkg/client/` Cloud Download API | ❌ 未封装 | `cmd/sclient/cloud_*.go` 全部使用裸 HTTP 调用 |
| 归档云下载文件 | ❌ 无快捷端点 | 需手动构造 `.__cloud__/<taskID>/` 路径 |
| sclient 一键工作流 | ❌ 无 | 需多步手动操作 |
| E2E 测试 | ❌ 失败 | `TestE2E_SclientCLI` 需修复 |
| 数据完整性 | ✅ 完善 | SHA-256 checksum、原子写入、ChecksumStore |
| 加密隧道 | ✅ 成熟 | AES-256-GCM + mux 多路复用 |
| 安全防护 | ✅ 完整 | Bearer auth、SSRF 防护、路径穿越防护 |

### 1.3 成功标准

- Cloud Download API 完整封装到 `pkg/client/` 的 `Service` 接口
- sclient 的 `cloud_*` 命令通过 `clientfactory.Factory` 获取 `Service` 实例
- 新增快捷归档端点（单任务 + 批量）
- sclient `cloud-download` 支持 `--wait --archive --download --extract` 一键链式操作
- 所有新增代码有完整测试覆盖（单元测试 + 集成测试）
- E2E 测试修复通过
- 不破坏现有功能（所有现有测试通过）

## 2. 架构设计

### 2.1 新增/修改文件清单

```
# 新增文件
pkg/client/cloud.go                      — Cloud Download SDK 实现
pkg/client/cloud_test.go                 — Cloud Download SDK 测试
pkg/client/archivetask_test.go           — ArchiveCloudTask SDK 测试
cmd/sclient/cloud_download_test.go       — cloud-download 命令测试
cmd/sclient/cloud_archive.go             — cloud-archive 子命令
cmd/sclient/cloud_archive_test.go        — cloud-archive 命令测试
pkg/server/cloud_archive_handler.go      — 快捷归档端点 handler
pkg/server/cloud_archive_handler_test.go — 快捷归档端点测试

# 修改文件
pkg/client/client.go                     — Service 接口新增 Cloud + Archive 方法
cmd/sclient/cloud_download.go            — 改造为通过 Factory 使用 SDK，新增 --wait/--archive/--download/--extract flags
cmd/sclient/cloud_cancel.go              — 改造为通过 Factory 使用 SDK
cmd/sclient/cloud_list.go                — 改造为通过 Factory 使用 SDK
cmd/sclient/root.go                      — 注册 cloud-archive 子命令
pkg/server/handlers.go                   — 注册新路由
pkg/server/cloud_download_handler.go     — 可选：调整 handler 间共享逻辑
```

### 2.2 数据流

```
sclient CLI
  │
  ├── cloud_download.go ──→ clientfactory.Factory ──→ pkg/client/FileClient
  │                                                          │
  │                                                          ├── CloudDownload(ctx, url)    → POST /api/cloud/download
  │                                                          ├── CloudDownloadBatch(ctx,urls)→ POST /api/cloud/download/batch
  │                                                          ├── ListCloudTasks(ctx,filter)  → GET  /api/cloud/tasks
  │                                                          ├── GetCloudTask(ctx,id)       → GET  /api/cloud/tasks/{id}
  │                                                          ├── CancelCloudTask(ctx,id)    → POST /api/cloud/tasks/{id}/cancel
  │                                                          ├── DeleteCloudTask(ctx,id)    → DELETE /api/cloud/tasks/{id}
  │                                                          ├── ArchiveCloudTask(ctx,id)   → POST /api/cloud/tasks/{id}/archive
  │                                                          └── ArchiveCloudTasks(ctx,ids) → POST /api/cloud/archive
  │
  └── cloud_archive.go ──→ clientfactory.Factory ──→ pkg/client/FileClient
                                                          (同样使用上面的 ArchiveCloudTask/s 方法)
```

### 2.3 Service 接口扩展

```go
type Service interface {
    // ... 现有方法 ...

    // === Cloud Download ===
    CloudDownload(ctx context.Context, url string, opts ...CloudDownloadOption) (*CloudTask, error)
    CloudDownloadBatch(ctx context.Context, urls []string) ([]CloudTask, error)
    ListCloudTasks(ctx context.Context, status string) ([]CloudTask, error)
    GetCloudTask(ctx context.Context, taskID string) (*CloudTask, error)
    CancelCloudTask(ctx context.Context, taskID string) error
    DeleteCloudTask(ctx context.Context, taskID string) error

    // === Cloud Archive ===
    ArchiveCloudTask(ctx context.Context, taskID, archiveName string) (*ArchiveResult, error)
    ArchiveCloudTasks(ctx context.Context, taskIDs []string, archiveName string) (*ArchiveResult, error)
}
```

### 2.4 新增类型

```go
// CloudTask 表示一个云端下载任务
type CloudTask struct {
    ID          string     `json:"id"`
    URL         string     `json:"url"`
    Status      string     `json:"status"`
    Progress    float64    `json:"progress"`
    FileName    string     `json:"filename,omitempty"`
    FileSize    int64      `json:"file_size,omitempty"`
    Checksum    string     `json:"checksum,omitempty"`
    Error       string     `json:"error,omitempty"`
    CreatedAt   time.Time  `json:"created_at"`
    CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// CloudDownloadOption 配置云端下载行为
type CloudDownloadOption func(*cloudDownloadOptions)

// ArchiveResult 归档操作结果
type ArchiveResult struct {
    Success   bool   `json:"success"`
    File      string `json:"file"`
    Size      int64  `json:"size"`
    Checksum  string `json:"checksum"`
    TaskCount int    `json:"task_count,omitempty"`
}
```

## 3. 详细设计

### 3.1 Cloud Download SDK 实现

`pkg/client/cloud.go` 实现 `Service` 接口的 cloud download 方法：

```go
func (c *FileClient) CloudDownload(ctx context.Context, url string, opts ...CloudDownloadOption) (*CloudTask, error) {
    body := map[string]string{"url": url}
    // 应用选项
    cfg := &cloudDownloadOptions{}
    for _, opt := range opts {
        opt(cfg)
    }
    if cfg.filename != "" {
        body["filename"] = cfg.filename
    }

    var resp struct {
        Success bool       `json:"success"`
        Task    *CloudTask `json:"task"`
    }
    err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download", body, &resp)
    if err != nil {
        return nil, fmt.Errorf("cloud download: %w", err)
    }
    if !resp.Success {
        return nil, fmt.Errorf("cloud download: server returned success=false")
    }
    return resp.Task, nil
}
```

`doJSON` 是内部辅助方法，封装 JSON 序列化/反序列化 + HTTP 请求。

### 3.2 sclient cloud_* 命令改造

#### 当前状态

`cloud_download.go`、`cloud_cancel.go`、`cloud_list.go` 中：
- 手动构造 `http.Client` + `http.NewRequest` + `http.Response`
- 手动 `json.Marshal`/`json.Unmarshal`
- 不通过 `clientfactory.Factory`

#### 改造后

```go
// cloud_download.go
func NewCmdCloudDownload(factory *clientfactory.Factory) *cobra.Command {
    cmd := &cobra.Command{
        Use:   "cloud-download <url>...",
        Short: "下载远程文件到服务器",
        RunE: func(cmd *cobra.Command, args []string) error {
            client, err := factory.NewClient(cmd.Context())
            if err != nil {
                return err
            }
            wait, _ := cmd.Flags().GetBool("wait")
            archive, _ := cmd.Flags().GetBool("archive")
            download, _ := cmd.Flags().GetBool("download")
            extract, _ := cmd.Flags().GetBool("extract")

            // 创建任务
            tasks, err := client.CloudDownloadBatch(cmd.Context(), args)
            // ...

            if wait {
                // 轮询等待所有任务完成
                tasks = waitForCompletion(ctx, client, tasks)
                // 检查是否成功
                // ...

                if archive {
                    result, err := client.ArchiveCloudTasks(ctx, taskIDs, archiveName)
                    // ...
                    if download {
                        // 下载归档文件到本地
                        err := client.Download(ctx, result.File, outputPath)
                        // ...
                        if extract {
                            // 解压 tar.gz
                            err := extractTarGz(outputPath, outputDir)
                            // ...
                        }
                    }
                }
            }
            return nil
        },
    }

    cmd.Flags().Bool("wait", false, "等待所有任务完成")
    cmd.Flags().Bool("archive", false, "完成后打包归档")
    cmd.Flags().String("archive-name", "", "归档文件名")
    cmd.Flags().Bool("download", false, "下载归档到本地")
    cmd.Flags().String("output-dir", ".", "本地输出目录")
    cmd.Flags().Bool("extract", false, "解压归档（仅与 --download 同时使用）")
    return cmd
}
```

### 3.3 快捷归档端点

**`POST /api/cloud/tasks/{id}/archive`**

```go
func (h *CloudArchiveHandler) ArchiveTask(w http.ResponseWriter, r *http.Request) {
    taskID := r.PathValue("id")
    // 1. 验证任务存在且已完成
    task, err := h.manager.GetTask(r.Context(), taskID)
    if err != nil || task.Status != "completed" {
        writeJSON(w, http.StatusBadRequest, map[string]any{
            "success": false, "message": "task not found or not completed",
        })
        return
    }
    // 2. 获取任务关联的文件路径
    files := h.manager.GetTaskFiles(task)
    // 3. 调用 archive 逻辑打包
    archiveName := r.FormValue("archive_name") // 可选
    result, err := h.archiveService.Archive(r.Context(), files, archiveName)
    // 4. 返回结果
    writeJSON(w, http.StatusOK, result)
}
```

**校验逻辑：**
- 任务必须存在且 status 为 `completed`
- 任务关联的文件必须存在
- 归档文件名不能包含路径穿越

### 3.4 等待轮询逻辑

```go
func waitForCompletion(ctx context.Context, client client.Service, tasks []CloudTask) ([]CloudTask, error) {
    pending := make(map[string]bool)
    for _, t := range tasks {
        pending[t.ID] = true
    }

    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()

    for len(pending) > 0 {
        select {
        case <-ctx.Done():
            return nil, ctx.Err()
        case <-ticker.C:
            for id := range pending {
                task, err := client.GetCloudTask(ctx, id)
                if err != nil {
                    return nil, fmt.Errorf("poll task %s: %w", id, err)
                }
                switch task.Status {
                case "completed":
                    delete(pending, id)
                    fmt.Printf("  ✓ %s: completed (%s, %s)\n", id, task.FileName, formatSize(task.FileSize))
                case "failed", "cancelled":
                    delete(pending, id)
                    fmt.Printf("  ✗ %s: %s (%s)\n", id, task.Status, task.Error)
                default:
                    // 仍在下载中，显示进度
                    fmt.Printf("  ⟳ %s: %.1f%%\r", id, task.Progress*100)
                }
            }
        }
    }
    return tasks, nil
}
```

## 4. 测试策略

### 4.1 单元测试

| 测试文件 | 测试内容 | 方法 |
|----------|----------|------|
| `pkg/client/cloud_test.go` | CloudDownload SDK 方法 | `httptest` 模拟 server |
| `pkg/client/archivetask_test.go` | ArchiveCloudTask SDK 方法 | `httptest` 模拟 server |
| `cmd/sclient/cloud_download_test.go` | cloud-download 命令 | `Factory` mock + `CaptureStdout` |
| `cmd/sclient/cloud_archive_test.go` | cloud-archive 命令 | `Factory` mock + `CaptureStdout` |
| `pkg/server/cloud_archive_handler_test.go` | 快捷归档 handler | `httptest.NewRecorder` |

### 4.2 集成测试

| 测试文件 | 测试内容 | 方法 |
|----------|----------|------|
| `pkg/server/integration_test.go` | 新增端点集成 | `newTestServerWithAllRoutes` |

### 4.3 边界测试

- 空 URL 列表 → 返回 400
- 超过 100 URL → 返回 400
- 任务不存在时归档 → 返回 404
- 任务未完成时归档 → 返回 400
- 归档文件名包含路径穿越 → 拒绝
- 网络断开时的轮询超时 → 返回 ctx.Err()
- 大文件归档（>1GB）→ 流式处理，不 OOM

### 4.4 测试约束

遵循项目现有规范：
- 纯标准库测试（不使用 testify/gomock/gomega）
- 127.0.0.1 回环绑定
- Windows 兼容
- 全局状态隔离（t.Cleanup 恢复）
- Viper 隔离（`viper.New()` 独立实例）

## 5. E2E 测试修复

### 5.1 已知问题

`TestE2E_SclientCLI` 失败，错误为 `sclient list: no files found`。

### 5.2 修复方向

1. 检查 `test/e2e_test.go` 中 `startSPROXY` 的配置传递
2. 确认 sclient 子进程是否使用了正确的 `--config` 指向临时文件
3. 检查 `--server` flag 是否被本地 `~/.config/sproxy/sclient.yaml` 中的配置覆盖
4. 验证临时目录创建和清理逻辑

## 6. 安全考虑

| 风险 | 缓解措施 |
|------|----------|
| 归档路径穿越 | `ValidateFilePath` + `filepath.Clean` |
| 任务 ID 注入 | 仅访问当前用户的已存在任务 |
| 大文件 OOM | 流式归档（`io.Pipe`），不加载到内存 |
| 轮询 DDoS | 2 秒间隔，Context 超时 |
| 未授权访问 | 现有 auth 中间件覆盖新端点 |

## 7. 不涉及的范围

- 流式 API（`io.Reader`/`io.Writer`）— 非本次核心工作流必需
- zip 格式支持 — 当前仅 tar.gz 已满足需求
- 异步归档与进度通知 — 当前归档为同步操作，大文件归档由 HTTP 超时控制
- 分支清理 — 与功能开发无关