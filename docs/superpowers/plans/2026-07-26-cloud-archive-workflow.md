# Cloud Download → Archive → Download 全链路工作流实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将 Cloud Download API 封装到 `pkg/client/` 的 `Service` 接口，新增快捷归档端点，为 sclient 添加一键链式工作流（`--wait --archive --download --extract`），修复 E2E 测试。

**架构：**
- 服务端：新增 `POST /api/cloud/tasks/{id}/archive` 和 `POST /api/cloud/archive` 端点，将已完成云端下载任务的文件打包为 tar.gz
- 客户端 SDK：`pkg/client/cloud.go` 实现 Cloud Download 和 Cloud Archive 的 8 个方法，通过 `doRequest` 统一走直连/隧道
- CLI：改造 `cloud_download.go`/`cloud_list.go`/`cloud_cancel.go` 使用 `clientfactory.Factory` 获取 SDK 实例，新增 `cloud-archive` 子命令
- E2E 修复：设置 `XDG_CACHE_HOME` 为临时目录，隔离 `currentDir` 缓存

**技术栈：** Go 1.26, 标准库 `net/http`, `encoding/json`, `archive/tar`, `compress/gzip`, `crypto/sha256`

---

## 文件结构

### 新增文件

| 文件 | 职责 |
|------|------|
| `pkg/client/cloud.go` | Cloud Download + Cloud Archive SDK 实现（8 个方法） |
| `pkg/client/cloud_test.go` | Cloud SDK 的 httptest 模拟测试 |
| `pkg/server/cloud_archive_handler.go` | 快捷归档端点 handler（单任务 + 批量） |
| `pkg/server/cloud_archive_handler_test.go` | 快捷归档 handler 测试 |
| `cmd/sclient/cloud_archive.go` | `cloud-archive` 子命令 |

### 修改文件

| 文件 | 修改内容 |
|------|----------|
| `pkg/client/client.go` | `Service` 接口新增 8 个方法，`FileClient` 新增 `doJSON` 辅助方法 |
| `cmd/sclient/cloud_download.go` | 改造为通过 `Factory` 使用 SDK，新增 `--wait/--archive/--download/--extract/--archive-name/--output-dir` flags，新增 `waitForCompletion` 和 `extractTarGz` 辅助函数 |
| `cmd/sclient/cloud_list.go` | 改造为通过 `Factory` 使用 SDK |
| `cmd/sclient/cloud_cancel.go` | 改造为通过 `Factory` 使用 SDK |
| `cmd/sclient/root.go` | 注册 `cloud-archive` 子命令 |
| `pkg/server/handlers.go` | 注册新路由（localMux 和主 mux） |
| `test/e2e_extra_test.go` | 设置 `XDG_CACHE_HOME` 隔离 `currentDir` 缓存 |

---

### 任务 0：E2E 测试修复

**文件：**
- 修改：`test/e2e_extra_test.go:269-276`

- [ ] **步骤 1：分析根因**

当前 `TestE2E_SclientCLI` 失败，输出 `sclient list: no files found`。原因是：
1. `sclient list` 命令在 `PersistentPreRunE` 中调用 `loadCurrentDir()`，从 XDG cache 读取 `currentDir`
2. 如果用户本地 XDG cache 中有 `currentDir` 值（例如 `/some/subdir`），`list` 命令会过滤只显示该子目录下的文件
3. 上传的文件在根目录，所以过滤后显示为空

- [ ] **步骤 2：修复 E2E 测试**

```go
// 在 TestE2E_SclientCLI 中添加 XDG_CACHE_HOME 隔离
// 在创建 cfgPath 之前添加：
t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache"))
```

这确保 `loadCurrentDir()` 读取的是临时目录中的缓存，而不是用户本地的缓存。由于临时目录为空，`currentDir` 为 `""`，`list` 命令显示根目录文件。

- [ ] **步骤 3：运行测试验证通过**

```bash
cd test && go test -count=1 -run TestE2E_SclientCLI -v -timeout 120s
```

预期：PASS

- [ ] **步骤 4：Commit**

```bash
git add test/e2e_extra_test.go
git commit -m "fix: e2e test failure due to XDG cache currentDir isolation"
```

---

### 任务 1：Service 接口扩展 + doJSON 辅助方法

**文件：**
- 修改：`pkg/client/client.go`
- 测试：`pkg/client/cloud_test.go`（后续任务补充）

- [ ] **步骤 1：在 Service 接口中添加 Cloud Download + Cloud Archive 方法**

在 `pkg/client/client.go` 的 `Service` 接口中（`TunnelDo` 之前），新增：

```go
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
```

- [ ] **步骤 2：在 import 块中添加 `time` 包**

确保 `pkg/client/client.go` 的 import 中有 `"time"`（已有）。

- [ ] **步骤 3：在 FileClient 上添加 doJSON 辅助方法**

在 `doRequest` 方法之后（`closeBodyIfErr` 之前），添加：

```go
// doJSON 发送 JSON 请求体并解析 JSON 响应。
// 自动设置 Content-Type: application/json，并在非 2xx 时返回错误。
func (c *FileClient) doJSON(ctx context.Context, method, urlPath string, reqBody, respBody any) error {
    var bodyReader io.Reader
    if reqBody != nil {
        data, err := json.Marshal(reqBody)
        if err != nil {
            return fmt.Errorf("序列化请求体失败: %w", err)
        }
        bodyReader = bytes.NewReader(data)
    }

    headers := make(http.Header)
    headers.Set("Content-Type", "application/json")
    resp, err := c.doRequest(ctx, method, urlPath, bodyReader, headers)
    if err != nil {
        return fmt.Errorf("请求失败: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
        return fmt.Errorf("请求失败 (HTTP %d): %s", resp.StatusCode, string(body))
    }

    if respBody != nil {
        if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
            return fmt.Errorf("解析响应失败: %w", err)
        }
    }
    return nil
}
```

- [ ] **步骤 4：运行测试验证编译通过**

```bash
go build ./...
go vet ./pkg/client/...
```

预期：编译通过，无 vet 错误

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/client.go
git commit -m "feat: extend Service interface with Cloud Download and Cloud Archive methods"
```

---

### 任务 2：Cloud Download + Cloud Archive SDK 类型定义

**文件：**
- 创建：`pkg/client/cloud.go`（类型定义部分，方法实现在后续步骤）

- [ ] **步骤 1：创建 cloud.go 文件，定义类型和选项**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import "time"

// CloudTask 表示一个云端下载任务。
type CloudTask struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Filename   string     `json:"filename"`
	Status     string     `json:"status"`
	TotalSize  int64      `json:"total_size"`
	Downloaded int64      `json:"downloaded"`
	Checksum   string     `json:"checksum"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

// CloudDownloadOption 配置云端下载行为。
type CloudDownloadOption func(*cloudDownloadOptions)

type cloudDownloadOptions struct {
	filename string
}

// WithCloudDownloadFilename 设置云端下载的文件名（覆盖 URL 自动提取的文件名）。
func WithCloudDownloadFilename(name string) CloudDownloadOption {
	return func(o *cloudDownloadOptions) {
		o.filename = name
	}
}

// CloudBatchTaskResult 表示批量创建任务的结果条目。
type CloudBatchTaskResult struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// ArchiveResult 表示归档操作的结果。
type ArchiveResult struct {
	Success   bool   `json:"success"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	TaskCount int    `json:"task_count,omitempty"`
}
```

- [ ] **步骤 2：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 3：Commit**

```bash
git add pkg/client/cloud.go
git commit -m "feat: add CloudTask, ArchiveResult types and CloudDownloadOption"
```

---

### 任务 3：Cloud Download SDK 方法实现

**文件：**
- 创建：`pkg/client/cloud.go`（追加方法实现）

- [ ] **步骤 1：实现 CloudDownload（单 URL，同步/异步自动切换）**

```go
// CloudDownload 创建云端下载任务。
// 小文件（<20MB）同步完成，大文件异步执行。
func (c *FileClient) CloudDownload(ctx context.Context, url string, opts ...CloudDownloadOption) (*CloudTask, error) {
    cfg := &cloudDownloadOptions{}
    for _, opt := range opts {
        opt(cfg)
    }
    body := map[string]string{"url": url}
    if cfg.filename != "" {
        body["filename"] = cfg.filename
    }

    var task CloudTask
    if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download", body, &task); err != nil {
        return nil, fmt.Errorf("cloud download: %w", err)
    }
    return &task, nil
}
```

- [ ] **步骤 2：实现 CloudDownloadBatch（批量，始终异步）**

```go
// CloudDownloadBatch 批量创建云端下载任务（最多 100 URL）。
func (c *FileClient) CloudDownloadBatch(ctx context.Context, urls []string) ([]CloudTask, error) {
    entries := make([]map[string]string, len(urls))
    for i, u := range urls {
        entries[i] = map[string]string{"url": u}
    }
    body := map[string]any{"urls": entries}

    var result struct {
        Tasks []CloudTask `json:"tasks"`
    }
    if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download/batch", body, &result); err != nil {
        return nil, fmt.Errorf("cloud download batch: %w", err)
    }
    return result.Tasks, nil
}
```

- [ ] **步骤 3：实现 ListCloudTasks**

```go
// ListCloudTasks 列举云端下载任务。
// status 可选过滤：pending/downloading/completed/failed/cancelled，为空时返回全部。
func (c *FileClient) ListCloudTasks(ctx context.Context, status string) ([]CloudTask, error) {
    urlPath := "/api/cloud/tasks"
    if status != "" {
        urlPath += "?status=" + url.QueryEscape(status)
    }
    var tasks []CloudTask
    if err := c.doJSON(ctx, http.MethodGet, urlPath, nil, &tasks); err != nil {
        return nil, fmt.Errorf("list cloud tasks: %w", err)
    }
    return tasks, nil
}
```

- [ ] **步骤 4：实现 GetCloudTask**

```go
// GetCloudTask 查询单个任务详情。
func (c *FileClient) GetCloudTask(ctx context.Context, taskID string) (*CloudTask, error) {
    var task CloudTask
    if err := c.doJSON(ctx, http.MethodGet, "/api/cloud/tasks/"+url.PathEscape(taskID), nil, &task); err != nil {
        return nil, fmt.Errorf("get cloud task: %w", err)
    }
    return &task, nil
}
```

- [ ] **步骤 5：实现 CancelCloudTask 和 DeleteCloudTask**

```go
// CancelCloudTask 取消云端下载任务。
func (c *FileClient) CancelCloudTask(ctx context.Context, taskID string) error {
    return c.doJSON(ctx, http.MethodPost, "/api/cloud/tasks/"+url.PathEscape(taskID)+"/cancel", nil, nil)
}

// DeleteCloudTask 删除云端下载任务及关联文件。
func (c *FileClient) DeleteCloudTask(ctx context.Context, taskID string) error {
    return c.doJSON(ctx, http.MethodDelete, "/api/cloud/tasks/"+url.PathEscape(taskID), nil, nil)
}
```

- [ ] **步骤 6：实现 ArchiveCloudTask 和 ArchiveCloudTasks**

```go
// ArchiveCloudTask 将单任务文件打包为 tar.gz 并存放到 uploads 目录。
func (c *FileClient) ArchiveCloudTask(ctx context.Context, taskID, archiveName string) (*ArchiveResult, error) {
    body := map[string]string{"archive_name": archiveName}
    var result ArchiveResult
    if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/tasks/"+url.PathEscape(taskID)+"/archive", body, &result); err != nil {
        return nil, fmt.Errorf("archive cloud task: %w", err)
    }
    return &result, nil
}

// ArchiveCloudTasks 将多个任务的文件打包为一个 tar.gz。
func (c *FileClient) ArchiveCloudTasks(ctx context.Context, taskIDs []string, archiveName string) (*ArchiveResult, error) {
    body := map[string]any{
        "task_ids":     taskIDs,
        "archive_name": archiveName,
    }
    var result ArchiveResult
    if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/archive", body, &result); err != nil {
        return nil, fmt.Errorf("archive cloud tasks: %w", err)
    }
    return &result, nil
}
```

- [ ] **步骤 7：确保 import 包含 `"net/url"`**

在 `pkg/client/cloud.go` 的 import 中添加 `"net/url"`。

- [ ] **步骤 8：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 9：Commit**

```bash
git add pkg/client/cloud.go
git commit -m "feat: implement Cloud Download and Cloud Archive SDK methods"
```

---

### 任务 4：Cloud SDK 测试

**文件：**
- 创建：`pkg/client/cloud_test.go`

- [ ] **步骤 1：创建 cloud_test.go 文件，编写测试**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cloudTestServer 返回一个模拟 cloud download handler 的测试服务器。
func cloudTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	mux := http.NewServeMux()

	// POST /api/cloud/download — 创建单任务
	mux.HandleFunc("POST /api/cloud/download", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL      string `json:"url"`
			Filename string `json:"filename,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
			return
		}
		task := CloudTask{
			ID:        "test-task-1",
			URL:       req.URL,
			Filename:  req.Filename,
			Status:    "pending",
			CreatedAt: time.Now(),
		}
		if task.Filename == "" {
			task.Filename = "download"
		}
		json.NewEncoder(w).Encode(task)
	})

	// POST /api/cloud/download/batch — 批量创建
	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URLs []map[string]string `json:"urls"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		tasks := make([]CloudTask, 0, len(req.URLs))
		for i, entry := range req.URLs {
			tasks = append(tasks, CloudTask{
				ID:       fmt.Sprintf("task-%d", i+1),
				URL:      entry["url"],
				Filename: "download",
				Status:   "pending",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
	})

	// GET /api/cloud/tasks — 列表
	mux.HandleFunc("GET /api/cloud/tasks", func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		tasks := []CloudTask{
			{ID: "task-1", URL: "https://example.com/a.zip", Filename: "a.zip", Status: "completed"},
			{ID: "task-2", URL: "https://example.com/b.zip", Filename: "b.zip", Status: "downloading"},
		}
		if status != "" {
			filtered := make([]CloudTask, 0)
			for _, t := range tasks {
				if t.Status == status {
					filtered = append(filtered, t)
				}
			}
			json.NewEncoder(w).Encode(filtered)
			return
		}
		json.NewEncoder(w).Encode(tasks)
	})

	// GET /api/cloud/tasks/{id} — 单个任务
	mux.HandleFunc("GET /api/cloud/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(CloudTask{
			ID:       id,
			URL:      "https://example.com/file.zip",
			Filename: "file.zip",
			Status:   "completed",
		})
	})

	// POST /api/cloud/tasks/{id}/cancel
	mux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
	})

	// DELETE /api/cloud/tasks/{id}
	mux.HandleFunc("DELETE /api/cloud/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})

	// POST /api/cloud/tasks/{id}/archive
	mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(ArchiveResult{
			Success: true,
			File:    "archive.tar.gz",
			Size:    1024,
			Checksum: "abc123",
			TaskCount: 1,
		})
	})

	// POST /api/cloud/archive
	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ArchiveResult{
			Success:   true,
			File:      "combined.tar.gz",
			Size:      2048,
			Checksum:  "def456",
			TaskCount: 2,
		})
	})

	ts := httptest.NewServer(mux)
	return ts, ts.URL
}

func TestCloudDownload_CreateTask(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	task, err := client.CloudDownload(context.Background(), "https://example.com/file.zip")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "test-task-1" {
		t.Fatalf("expected task ID 'test-task-1', got %q", task.ID)
	}
	if task.Status != "pending" {
		t.Fatalf("expected status 'pending', got %q", task.Status)
	}
}

func TestCloudDownload_CreateTaskWithFilename(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	task, err := client.CloudDownload(context.Background(), "https://example.com/file.zip",
		WithCloudDownloadFilename("myfile.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Filename != "myfile.zip" {
		t.Fatalf("expected filename 'myfile.zip', got %q", task.Filename)
	}
}

func TestCloudDownload_EmptyURL(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	_, err := client.CloudDownload(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestCloudDownload_Batch(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	urls := []string{"https://example.com/a.zip", "https://example.com/b.zip"}
	tasks, err := client.CloudDownloadBatch(context.Background(), urls)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestCloudDownload_ListTasks(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	tasks, err := client.ListCloudTasks(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestCloudDownload_ListTasksFiltered(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	tasks, err := client.ListCloudTasks(context.Background(), "completed")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(tasks))
	}
	if tasks[0].Status != "completed" {
		t.Fatalf("expected status 'completed', got %q", tasks[0].Status)
	}
}

func TestCloudDownload_GetTask(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	task, err := client.GetCloudTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Fatalf("expected task ID 'task-1', got %q", task.ID)
	}
}

func TestCloudDownload_GetTaskNotFound(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	_, err := client.GetCloudTask(context.Background(), "notfound")
	if err == nil {
		t.Fatal("expected error for not found task")
	}
}

func TestCloudDownload_CancelTask(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.CancelCloudTask(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
}

func TestCloudDownload_CancelTaskNotFound(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.CancelCloudTask(context.Background(), "notfound"); err == nil {
		t.Fatal("expected error for not found task")
	}
}

func TestCloudDownload_DeleteTask(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.DeleteCloudTask(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
}

func TestCloudDownload_DeleteTaskNotFound(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.DeleteCloudTask(context.Background(), "notfound"); err == nil {
		t.Fatal("expected error for not found task")
	}
}

func TestCloudArchive_ArchiveTask(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	result, err := client.ArchiveCloudTask(context.Background(), "task-1", "my-archive")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if result.File != "archive.tar.gz" {
		t.Fatalf("expected file 'archive.tar.gz', got %q", result.File)
	}
}

func TestCloudArchive_ArchiveTaskNotFound(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	_, err := client.ArchiveCloudTask(context.Background(), "notfound", "archive")
	if err == nil {
		t.Fatal("expected error for not found task")
	}
}

func TestCloudArchive_ArchiveTasks(t *testing.T) {
	ts, url := cloudTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	result, err := client.ArchiveCloudTasks(context.Background(), []string{"task-1", "task-2"}, "combined")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatal("expected success")
	}
	if result.TaskCount != 2 {
		t.Fatalf("expected 2 tasks, got %d", result.TaskCount)
	}
}
```

注意：需要在 `cloud_test.go` 的 import 中添加 `"fmt"`。

- [ ] **步骤 2：运行测试验证通过**

```bash
go test -count=1 -v ./pkg/client/... -run TestCloud
```

预期：所有测试 PASS

- [ ] **步骤 3：Commit**

```bash
git add pkg/client/cloud_test.go
git commit -m "test: add Cloud Download and Cloud Archive SDK tests"
```

---

### 任务 5：服务端快捷归档 Handler

**文件：**
- 创建：`pkg/server/cloud_archive_handler.go`

- [ ] **步骤 1：创建 cloud_archive_handler.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CloudArchiveRequest 是 POST /api/cloud/tasks/{id}/archive 的请求体。
type CloudArchiveRequest struct {
	ArchiveName string `json:"archive_name,omitempty"`
}

// CloudArchiveBatchRequest 是 POST /api/cloud/archive 的请求体。
type CloudArchiveBatchRequest struct {
	TaskIDs     []string `json:"task_ids"`
	ArchiveName string   `json:"archive_name,omitempty"`
}

// CloudArchiveResult 是归档操作的响应结构体。
type CloudArchiveResult struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	File      string `json:"file,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Checksum  string `json:"checksum,omitempty"`
	TaskCount int    `json:"task_count,omitempty"`
}

// cloudArchiveTask 处理 POST /api/cloud/tasks/{id}/archive。
// 将指定已完成任务的文件打包为 tar.gz 输出到 uploads 目录。
func (h *Handlers) cloudArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")

	// 1. 验证任务存在且已完成
	task, ok := h.cloudMgr.GetTask(taskID)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task not found"}, http.StatusNotFound)
		return
	}
	if task.Status != "completed" {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task is not completed"}, http.StatusBadRequest)
		return
	}
	if task.Filename == "" {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task has no file"}, http.StatusBadRequest)
		return
	}

	// 2. 解析请求体
	var req CloudArchiveRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}

	// 3. 构造文件路径
	cloudDir := filepath.Join(h.cloudMgr.UploadsDir(), cloudDirName)
	taskDir := filepath.Join(cloudDir, taskID)
	sourceFile := filepath.Join(taskDir, task.Filename)

	// 验证文件在安全路径内
	if !strings.HasPrefix(filepath.Clean(sourceFile), filepath.Clean(cloudDir)+string(filepath.Separator)) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid file path"}, http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(sourceFile); os.IsNotExist(err) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "downloaded file not found"}, http.StatusNotFound)
		return
	}

	// 4. 生成归档文件名
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-task-%s-%d.tar.gz", taskID, time.Now().Unix())
	}
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}
	// 路径穿越防护
	archiveName = filepath.Base(archiveName)
	if archiveName == "" {
		archiveName = "archive.tar.gz"
	}

	outputPath := filepath.Join(h.cloudMgr.UploadsDir(), archiveName)

	// 5. 流式打包
	if err := createTarGz(sourceFile, task.Filename, outputPath, h.logger); err != nil {
		h.logger.Error("cloud archive failed", "task_id", taskID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive failed"}, http.StatusInternalServerError)
		return
	}

	// 6. 计算 checksum
	cs, err := h.checksumStore.Get(r.Context(), archiveName)
	if err != nil {
		// 如果 checksum store 不可用，返回空
		cs = ""
	}

	// 7. 获取文件大小
	info, _ := os.Stat(outputPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:   true,
		File:      archiveName,
		Size:      size,
		Checksum:  cs,
		TaskCount: 1,
	}, http.StatusOK)
}

// cloudArchiveBatch 处理 POST /api/cloud/archive。
// 将多个已完成任务的文件打包为一个 tar.gz。
func (h *Handlers) cloudArchiveBatch(w http.ResponseWriter, r *http.Request) {
	var req CloudArchiveBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid request body"}, http.StatusBadRequest)
		return
	}
	if len(req.TaskIDs) == 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "task_ids is required"}, http.StatusBadRequest)
		return
	}
	if len(req.TaskIDs) > 100 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "maximum 100 tasks"}, http.StatusBadRequest)
		return
	}

	// 收集所有已完成任务的文件路径
	cloudDir := filepath.Join(h.cloudMgr.UploadsDir(), cloudDirName)
	type fileEntry struct {
		fullPath string
		relPath  string
	}
	var files []fileEntry

	for _, taskID := range req.TaskIDs {
		task, ok := h.cloudMgr.GetTask(taskID)
		if !ok {
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("task %s not found", taskID),
			}, http.StatusNotFound)
			return
		}
		if task.Status != "completed" {
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("task %s is not completed", taskID),
			}, http.StatusBadRequest)
			return
		}
		if task.Filename == "" {
			continue
		}

		taskDir := filepath.Join(cloudDir, taskID)
		sourceFile := filepath.Join(taskDir, task.Filename)

		if !strings.HasPrefix(filepath.Clean(sourceFile), filepath.Clean(cloudDir)+string(filepath.Separator)) {
			continue
		}
		if _, err := os.Stat(sourceFile); os.IsNotExist(err) {
			continue
		}

		files = append(files, fileEntry{
			fullPath: sourceFile,
			relPath:  task.Filename,
		})
	}

	if len(files) == 0 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "no valid files to archive"}, http.StatusBadRequest)
		return
	}

	// 生成归档文件名
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("cloud-batch-%d.tar.gz", time.Now().Unix())
	}
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}
	archiveName = filepath.Base(archiveName)
	if archiveName == "" {
		archiveName = "archive.tar.gz"
	}

	outputPath := filepath.Join(h.cloudMgr.UploadsDir(), archiveName)

	// 流式打包多个文件
	if err := createMultiFileTarGz(files, outputPath, h.logger); err != nil {
		h.logger.Error("cloud archive batch failed", "error", err)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive failed"}, http.StatusInternalServerError)
		return
	}

	cs, _ := h.checksumStore.Get(r.Context(), archiveName)
	info, _ := os.Stat(outputPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	sendJSONResponse(w, CloudArchiveResult{
		Success:   true,
		File:      archiveName,
		Size:      size,
		Checksum:  cs,
		TaskCount: len(files),
	}, http.StatusOK)
}

// createTarGz 将单个文件打包为 tar.gz。
func createTarGz(sourceFile, sourceName, outputPath string, logger *slog.Logger) error {
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	file, err := os.Open(sourceFile)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("create tar header: %w", err)
	}
	header.Name = filepath.ToSlash(sourceName)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("write file content: %w", err)
	}

	return nil
}

// createMultiFileTarGz 将多个文件打包为 tar.gz。
func createMultiFileTarGz(files []fileEntry, outputPath string, logger *slog.Logger) error {
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer out.Close()

	gw := gzip.NewWriter(out)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, f := range files {
		if err := addFileToTar(tw, f.fullPath, f.relPath, logger); err != nil {
			logger.Warn("add file to tar failed", "path", f.relPath, "error", err)
		}
	}

	return nil
}

// 需要将 addFileToTar 从 archive.go 中提取为可复用函数，或在此处定义。
// 注意：archive.go 中已有 addFileToTar，但它是包内私有的，cloud_archive_handler.go 可以访问。
```

注意：`addFileToTar` 已在 `pkg/server/archive.go` 中定义，同包可直接使用。

- [ ] **步骤 2：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 3：Commit**

```bash
git add pkg/server/cloud_archive_handler.go
git commit -m "feat: add cloud archive handler endpoints (single task + batch)"
```

---

### 任务 6：注册新路由

**文件：**
- 修改：`pkg/server/handlers.go`

- [ ] **步骤 1：在 localMux 和主 mux 中注册新路由**

在 `pkg/server/handlers.go` 的 `RegisterRoutes` 函数中，找到云端下载 API 路由注册区域（`handlers.go` 中 `// 云端下载 API（localMux：隧道认证）`部分），在 `localMux` 和主 `mux` 中各添加两行：

```go
// 在 localMux 的路由中添加（在 DELETE /api/cloud/tasks/{id} 之后）：
localMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
localMux.HandleFunc("POST /api/cloud/archive", h.cloudArchiveBatch)

// 在主 mux 的路由中同样添加（带上 authMiddleware）：
mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.authMiddleware(h.cloudArchiveTask))
mux.HandleFunc("POST /api/cloud/archive", h.authMiddleware(h.cloudArchiveBatch))
```

- [ ] **步骤 2：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 3：Commit**

```bash
git add pkg/server/handlers.go
git commit -m "feat: register cloud archive routes in localMux and main mux"
```

---

### 任务 7：服务端快捷归档 Handler 测试

**文件：**
- 创建：`pkg/server/cloud_archive_handler_test.go`

- [ ] **步骤 1：创建测试文件**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupCloudArchiveTest(t *testing.T) (*httptest.Server, *CloudDownloadManager, string) {
	t.Helper()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() {
		mgr.StopFlush()
		os.RemoveAll(filepath.Join(dir, ".__cloud__"))
		os.RemoveAll(filepath.Join(dir, ".__downloads__"))
	})

	h := &Handlers{cloudMgr: mgr, logger: testLogger()}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
	mux.HandleFunc("POST /api/cloud/archive", h.cloudArchiveBatch)
	return httptest.NewServer(mux), mgr, dir
}

// createCompletedTask 创建一个已完成的任务，并在 __cloud__/<id>/ 下创建测试文件。
func createCompletedTask(t *testing.T, mgr *CloudDownloadManager, filename string) string {
	t.Helper()
	task, err := mgr.CreateTask("url", "https://example.com/"+filename, filename, 100)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = "completed"

	// 创建任务文件
	cloudDir := filepath.Join(mgr.UploadsDir(), cloudDirName)
	taskDir := filepath.Join(cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, filename), []byte("test content"), 0644); err != nil {
		t.Fatal(err)
	}

	return task.ID
}

func TestCloudArchive_SingleTask(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	taskID := createCompletedTask(t, mgr, "testfile.txt")

	body := strings.NewReader(`{"archive_name": "my-archive"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+taskID+"/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success, got message: %s", result.Message)
	}
	if result.File == "" {
		t.Fatal("expected non-empty file name")
	}
	if result.Size <= 0 {
		t.Fatal("expected positive size")
	}
	if result.TaskCount != 1 {
		t.Fatalf("expected TaskCount=1, got %d", result.TaskCount)
	}

	// 验证归档文件已创建
	archivePath := filepath.Join(mgr.UploadsDir(), result.File)
	if _, err := os.Stat(archivePath); os.IsNotExist(err) {
		t.Fatalf("archive file not created at %s", archivePath)
	}
}

func TestCloudArchive_TaskNotFound(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	body := strings.NewReader(`{"archive_name": "archive"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/nonexistent/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCloudArchive_TaskNotCompleted(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	// 不设置为 completed

	body := strings.NewReader(`{"archive_name": "archive"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCloudArchive_BatchTasks(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	taskID1 := createCompletedTask(t, mgr, "file1.txt")
	taskID2 := createCompletedTask(t, mgr, "file2.txt")

	body := strings.NewReader(`{"task_ids": ["` + taskID1 + `","` + taskID2 + `"], "archive_name": "combined"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success: %s", result.Message)
	}
	if result.TaskCount != 2 {
		t.Fatalf("expected TaskCount=2, got %d", result.TaskCount)
	}
}

func TestCloudArchive_BatchEmptyTaskIDs(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	body := strings.NewReader(`{"task_ids": [], "archive_name": "empty"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCloudArchive_BatchTaskNotFound(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	body := strings.NewReader(`{"task_ids": ["nonexistent"], "archive_name": "test"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCloudArchive_DefaultArchiveName(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	taskID := createCompletedTask(t, mgr, "testfile.txt")

	// 不指定 archive_name，使用默认名称
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+taskID+"/archive", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success")
	}
	if result.File == "" {
		t.Fatal("expected non-empty file name")
	}
}
```

- [ ] **步骤 2：运行测试验证通过**

```bash
go test -count=1 -v ./pkg/server/... -run TestCloudArchive
```

预期：所有测试 PASS

- [ ] **步骤 3：Commit**

```bash
git add pkg/server/cloud_archive_handler_test.go
git commit -m "test: add cloud archive handler tests"
```

---

### 任务 8：改造 sclient cloud_download.go

**文件：**
- 修改：`cmd/sclient/cloud_download.go`

- [ ] **步骤 1：重写 NewCmdCloudDownload 使用 Factory 获取 SDK**

```go
// NewCmdCloudDownload 创建云端下载命令的工厂函数。
func NewCmdCloudDownload(factory clientfactory.Factory, ios cli.IOStreams, st *state.State, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-download <url> [url...]",
		Short: "从云端下载文件（服务端先拉取，再下载到本地）",
		Long: `通过 sproxy 服务端从外部 URL 下载文件，完成后自动下载到本地并清理云端副本。

小文件（< 20 MiB）默认同步等待，大文件自动切换异步模式。
如果同步下载过程中连接断开，服务端自动转为异步模式继续下载。

支持多个 URL 参数或通过 --batch 从文件读取 URL 列表。
使用 --wait 等待所有任务完成后自动进入归档/下载/解压链式操作。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			wait, _ := cmd.Flags().GetBool("wait")
			archive, _ := cmd.Flags().GetBool("archive")
			download, _ := cmd.Flags().GetBool("download")
			extract, _ := cmd.Flags().GetBool("extract")
			archiveName, _ := cmd.Flags().GetString("archive-name")
			outputDir, _ := cmd.Flags().GetString("output-dir")
			noCleanup, _ := cmd.Flags().GetBool("no-cleanup")
			pollInterval, _ := cmd.Flags().GetDuration("poll-interval")
			forceAsync, _ := cmd.Flags().GetBool("force-async")
			batchFile, _ := cmd.Flags().GetString("batch")

			// 收集所有 URL
			urls := args
			if batchFile != "" {
				fileURLs, err := readURLsFromFile(batchFile)
				if err != nil {
					return fmt.Errorf("读取 batch 文件失败: %w", err)
				}
				urls = append(urls, fileURLs...)
			}
			if len(urls) == 0 {
				return fmt.Errorf("未指定下载 URL，请提供 URL 参数或使用 --batch 指定文件")
			}

			// 创建任务
			ios.WriteOutLine("创建云端下载任务...")
			tasks, err := svc.CloudDownloadBatch(cmd.Context(), urls)
			if err != nil {
				return fmt.Errorf("创建云端下载任务失败: %w", err)
			}

			// 输出任务信息
			for _, t := range tasks {
				statusLine := fmt.Sprintf("  %s: %s", t.ID, t.Status)
				if t.Filename != "" {
					statusLine += fmt.Sprintf(" (%s)", t.Filename)
				}
				if t.Error != "" {
					statusLine += fmt.Sprintf(" - %s", t.Error)
				}
				ios.WriteOutLine(statusLine)
			}

			// 等待模式
			if wait {
				completedTasks, waitErr := waitForCompletion(cmd.Context(), svc, ios, tasks, pollInterval)
				if waitErr != nil {
					return waitErr
				}

				// 检查是否有成功任务
				var succeededIDs []string
				var succeededTask *client.CloudTask
				for _, t := range completedTasks {
					if t.Status == "completed" {
						succeededIDs = append(succeededIDs, t.ID)
						if succeededTask == nil {
							succeededTask = &t
						}
					}
				}

				if len(succeededIDs) == 0 {
					return fmt.Errorf("所有任务均未成功完成")
				}

				// 归档模式
				if archive {
					name := archiveName
					if name == "" {
						if len(succeededIDs) == 1 && succeededTask != nil {
							name = succeededTask.Filename
						} else {
							name = fmt.Sprintf("cloud-batch-%d", time.Now().Unix())
						}
					}

					ios.WriteOutLine("打包归档中...")
					var result *client.ArchiveResult
					if len(succeededIDs) == 1 {
						result, err = svc.ArchiveCloudTask(cmd.Context(), succeededIDs[0], name)
					} else {
						result, err = svc.ArchiveCloudTasks(cmd.Context(), succeededIDs, name)
					}
					if err != nil {
						return fmt.Errorf("归档失败: %w", err)
					}

					ios.WriteOutLine("  归档完成: %s (%d bytes)", result.File, result.Size)

					// 下载模式
					if download {
						outputPath := filepath.Join(outputDir, result.File)
						ios.WriteOutLine("下载归档到本地: %s", outputPath)

						if err := svc.Download(cmd.Context(), result.File, outputPath); err != nil {
							return fmt.Errorf("下载归档失败: %w", err)
						}

						ios.WriteOutLine("  下载完成")

						// 解压模式
						if extract {
							ios.WriteOutLine("解压中...")
							if err := extractTarGz(outputPath, outputDir); err != nil {
								return fmt.Errorf("解压失败: %w", err)
							}
							ios.WriteOutLine("  解压完成: %s", outputDir)
						}
					}
				}

				// 非归档模式下的下载
				if download && !archive {
					for _, t := range completedTasks {
						if t.Status != "completed" {
							continue
						}
						outputPath := filepath.Join(outputDir, t.Filename)
						ios.WriteOutLine("下载: %s -> %s", t.Filename, outputPath)
						cloudPath := ".__cloud__/" + t.ID + "/" + t.Filename
						if err := svc.Download(cmd.Context(), cloudPath, outputPath); err != nil {
							ios.WriteErrLine("  下载失败: %v", err)
							continue
						}
						// 清理云端副本
						if !noCleanup {
							_ = svc.Delete(cmd.Context(), cloudPath, "")
						}
					}
				}

				// 清理云端任务
				if !noCleanup {
					for _, id := range succeededIDs {
						_ = svc.DeleteCloudTask(cmd.Context(), id)
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().Bool("force-async", false, "强制使用异步模式")
	cmd.Flags().Bool("no-cleanup", false, "不清理云端副本")
	cmd.Flags().Duration("poll-interval", 2*time.Second, "异步模式轮询间隔")
	cmd.Flags().String("batch", "", "从文件读取 URL 列表")
	cmd.Flags().Bool("wait", false, "等待所有任务完成")
	cmd.Flags().Bool("archive", false, "完成后打包归档")
	cmd.Flags().String("archive-name", "", "归档文件名")
	cmd.Flags().Bool("download", false, "下载归档到本地")
	cmd.Flags().String("output-dir", ".", "本地输出目录")
	cmd.Flags().Bool("extract", false, "解压归档（仅与 --download 同时使用）")

	cmd.AddCommand(NewCmdCloudList(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdCloudCancel(factory, ios, cfgSvc))

	return cmd
}
```

- [ ] **步骤 2：添加 waitForCompletion 辅助函数**

```go
// waitForCompletion 轮询等待所有云端任务完成。
func waitForCompletion(ctx context.Context, svc client.Service, ios cli.IOStreams, tasks []client.CloudTask, interval time.Duration) ([]client.CloudTask, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}

	pending := make(map[string]client.CloudTask)
	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "downloading" {
			pending[t.ID] = t
		}
	}

	// 如果没有待处理任务，直接返回
	if len(pending) == 0 {
		return tasks, nil
	}

	ios.WriteOutLine("等待 %d 个任务完成...", len(pending))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	results := make([]client.CloudTask, 0, len(tasks))
	for _, t := range tasks {
		if t.Status == "pending" || t.Status == "downloading" {
			continue // 稍后更新
		}
		results = append(results, t)
	}

	for len(pending) > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			for id := range pending {
				task, err := svc.GetCloudTask(ctx, id)
				if err != nil {
					ios.WriteErrLine("  轮询任务 %s 失败: %v", id, err)
					continue
				}

				switch task.Status {
				case "completed":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteOutLine("  ✓ %s: 完成 (%s, %d bytes)", id, task.Filename, task.TotalSize)
				case "failed":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteErrLine("  ✗ %s: 失败 - %s", id, task.Error)
				case "cancelled":
					delete(pending, id)
					results = append(results, *task)
					ios.WriteErrLine("  ✗ %s: 已取消", id)
				default:
					// 下载中，显示进度
					pct := int64(0)
					if task.TotalSize > 0 {
						pct = task.Downloaded * 100 / task.TotalSize
					}
					ios.WriteOutLine("  ⟳ %s: %d%% (%d/%d bytes)", id, pct, task.Downloaded, task.TotalSize)
				}
			}
		}
	}

	return results, nil
}
```

- [ ] **步骤 3：添加 extractTarGz 辅助函数**

```go
// extractTarGz 解压 tar.gz 文件到指定目录。
func extractTarGz(src, destDir string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开归档文件失败: %w", err)
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("创建 gzip reader 失败: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 头失败: %w", err)
		}

		// 路径穿越防护
		targetPath := filepath.Join(destDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(targetPath, filepath.Clean(destDir)+string(filepath.Separator)) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
			outFile, err := os.Create(targetPath)
			if err != nil {
				return fmt.Errorf("创建文件失败: %w", err)
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return fmt.Errorf("写入文件失败: %w", err)
			}
			outFile.Close()
		}
	}

	return nil
}
```

- [ ] **步骤 4：更新 import 块**

确保 `cloud_download.go` 的 import 包含：
```go
import (
    "archive/tar"
    "compress/gzip"
    "path/filepath"
    "strings"
    "time"

    "github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
    "github.com/cocomhub/sproxy/cmd/sclient/internal/state"
    "github.com/cocomhub/sproxy/pkg/cli"
    "github.com/cocomhub/sproxy/pkg/client"
    "github.com/spf13/cobra"
)
```

移除不再需要的 `"bytes"`, `"crypto/sha256"`, `"encoding/hex"`, `"io"`, `"net/url"` 等（如果不再使用）。

- [ ] **步骤 5：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 6：Commit**

```bash
git add cmd/sclient/cloud_download.go
git commit -m "feat: refactor cloud-download to use SDK, add --wait/--archive/--download/--extract flags"
```

---

### 任务 9：改造 sclient cloud_list.go 和 cloud_cancel.go

**文件：**
- 修改：`cmd/sclient/cloud_list.go`
- 修改：`cmd/sclient/cloud_cancel.go`

- [ ] **步骤 1：重写 NewCmdCloudList 使用 SDK**

```go
// NewCmdCloudList 创建 cloud list 命令的工厂函数。
func NewCmdCloudList(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出所有云端下载任务",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			statusFilter, _ := cmd.Flags().GetString("status")
			tasks, err := svc.ListCloudTasks(cmd.Context(), statusFilter)
			if err != nil {
				return fmt.Errorf("获取云端下载任务列表失败: %w", err)
			}

			fm := buildFormatterWithWriter(ios.Out, cmd)
			if len(tasks) == 0 {
				fm.Println("暂无云端下载任务")
				return nil
			}

			// 将 client.CloudTask 转换为 cloudTaskInfo 用于格式化输出
			infos := make([]cloudTaskInfo, len(tasks))
			for i, t := range tasks {
				infos[i] = cloudTaskInfo(t)
			}
			fm.PrintCloudTaskList(infos)
			return nil
		},
	}

	cmd.Flags().String("status", "", "按状态过滤（pending/downloading/completed/failed/cancelled）")

	return cmd
}
```

注意：`cloudTaskInfo` 是 `cloudTaskResponse` 的类型别名，已在 `cloud_list.go` 中定义。由于 `CloudTask` 的字段与 `cloudTaskResponse` 几乎一致，可以强制转换（`cloudTaskInfo(t)` 依赖底层字段匹配，如果字段名有差异需要调整）。

- [ ] **步骤 2：重写 NewCmdCloudCancel 使用 SDK**

```go
// NewCmdCloudCancel 创建 cloud cancel 命令的工厂函数。
func NewCmdCloudCancel(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "取消云端下载任务",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			taskID := args[0]
			fm := buildFormatterWithWriter(ios.Out, cmd)

			if err := svc.CancelCloudTask(cmd.Context(), taskID); err != nil {
				fm.PrintCloudTaskCancelResult(taskID, false, err.Error())
				return nil
			}

			fm.PrintCloudTaskCancelResult(taskID, true, "已取消")
			return nil
		},
	}

	return cmd
}
```

- [ ] **步骤 3：清理 cloud_list.go 和 cloud_cancel.go 中不再使用的 import**

移除 `cloud_list.go` 中不再需要的 `"encoding/json"`, `"io"`, `"net/http"`, `"net/url"`。
移除 `cloud_cancel.go` 中不再需要的 `"encoding/json"`, `"io"`, `"net/http"`, `"net/url"`。

- [ ] **步骤 4：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/cloud_list.go cmd/sclient/cloud_cancel.go
git commit -m "feat: refactor cloud-list and cloud-cancel to use SDK"
```

---

### 任务 10：新增 cloud-archive 子命令 + 注册到 root

**文件：**
- 创建：`cmd/sclient/cloud_archive.go`
- 修改：`cmd/sclient/root.go`

- [ ] **步骤 1：创建 cloud_archive.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// NewCmdCloudArchive 创建 cloud-archive 命令的工厂函数。
func NewCmdCloudArchive(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud-archive <task-id> [task-id...]",
		Short: "打包云端下载已完成的任务文件",
		Long: `将指定已完成云端下载任务的文件打包为 tar.gz 并存放到服务端 uploads 目录。

支持单个或多个任务 ID。使用 --name 指定归档文件名。`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				ios.WriteErrLine("初始化客户端失败: %v", err)
				return fmt.Errorf(errFmtInitClient, err)
			}

			archiveName, _ := cmd.Flags().GetString("name")

			var result interface{}
			if len(args) == 1 {
				result, err = svc.ArchiveCloudTask(cmd.Context(), args[0], archiveName)
			} else {
				result, err = svc.ArchiveCloudTasks(cmd.Context(), args, archiveName)
			}
			if err != nil {
				return fmt.Errorf("归档失败: %w", err)
			}

			ios.WriteOutLine("归档完成: %+v", result)
			return nil
		},
	}

	cmd.Flags().String("name", "", "归档文件名（默认自动生成）")

	return cmd
}
```

- [ ] **步骤 2：在 root.go 中注册 cloud-archive 子命令**

在 `NewRootCmd` 函数中，`NewCmdCloudDownload` 注册之后添加：

```go
root.AddCommand(NewCmdCloudArchive(factory, ios, cfgSvc))
```

- [ ] **步骤 3：运行测试验证编译通过**

```bash
go build ./...
```

预期：编译通过

- [ ] **步骤 4：Commit**

```bash
git add cmd/sclient/cloud_archive.go cmd/sclient/root.go
git commit -m "feat: add cloud-archive subcommand for archiving completed cloud download tasks"
```

---

### 任务 11：cloud_* 命令测试

**文件：**
- 创建：`cmd/sclient/cloud_download_test.go`
- 创建：`cmd/sclient/cloud_archive_test.go`

- [ ] **步骤 1：创建 cloud_download_test.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// mockCloudService 实现 client.Service 接口，用于测试 cloud 命令。
type mockCloudService struct {
	client.Service
	cloudDownloadFn  func(ctx context.Context, url string, opts ...client.CloudDownloadOption) (*client.CloudTask, error)
	cloudBatchFn     func(ctx context.Context, urls []string) ([]client.CloudTask, error)
	listCloudTasksFn func(ctx context.Context, status string) ([]client.CloudTask, error)
	getCloudTaskFn   func(ctx context.Context, taskID string) (*client.CloudTask, error)
	cancelCloudFn    func(ctx context.Context, taskID string) error
	deleteCloudFn    func(ctx context.Context, taskID string) error
	archiveTaskFn    func(ctx context.Context, taskID, archiveName string) (*client.ArchiveResult, error)
	archiveTasksFn   func(ctx context.Context, taskIDs []string, archiveName string) (*client.ArchiveResult, error)
}

func (m *mockCloudService) CloudDownload(ctx context.Context, url string, opts ...client.CloudDownloadOption) (*client.CloudTask, error) {
	if m.cloudDownloadFn != nil {
		return m.cloudDownloadFn(ctx, url, opts...)
	}
	return &client.CloudTask{ID: "mock-task", Status: "pending"}, nil
}

func (m *mockCloudService) CloudDownloadBatch(ctx context.Context, urls []string) ([]client.CloudTask, error) {
	if m.cloudBatchFn != nil {
		return m.cloudBatchFn(ctx, urls)
	}
	tasks := make([]client.CloudTask, len(urls))
	for i, u := range urls {
		tasks[i] = client.CloudTask{ID: fmt.Sprintf("task-%d", i+1), URL: u, Status: "pending"}
	}
	return tasks, nil
}

func (m *mockCloudService) ListCloudTasks(ctx context.Context, status string) ([]client.CloudTask, error) {
	if m.listCloudTasksFn != nil {
		return m.listCloudTasksFn(ctx, status)
	}
	return nil, nil
}

func (m *mockCloudService) GetCloudTask(ctx context.Context, taskID string) (*client.CloudTask, error) {
	if m.getCloudTaskFn != nil {
		return m.getCloudTaskFn(ctx, taskID)
	}
	return &client.CloudTask{ID: taskID, Status: "completed"}, nil
}

func (m *mockCloudService) CancelCloudTask(ctx context.Context, taskID string) error {
	if m.cancelCloudFn != nil {
		return m.cancelCloudFn(ctx, taskID)
	}
	return nil
}

func (m *mockCloudService) DeleteCloudTask(ctx context.Context, taskID string) error {
	if m.deleteCloudFn != nil {
		return m.deleteCloudFn(ctx, taskID)
	}
	return nil
}

func (m *mockCloudService) ArchiveCloudTask(ctx context.Context, taskID, archiveName string) (*client.ArchiveResult, error) {
	if m.archiveTaskFn != nil {
		return m.archiveTaskFn(ctx, taskID, archiveName)
	}
	return &client.ArchiveResult{Success: true, File: "archive.tar.gz", Size: 1024, TaskCount: 1}, nil
}

func (m *mockCloudService) ArchiveCloudTasks(ctx context.Context, taskIDs []string, archiveName string) (*client.ArchiveResult, error) {
	if m.archiveTasksFn != nil {
		return m.archiveTasksFn(ctx, taskIDs, archiveName)
	}
	return &client.ArchiveResult{Success: true, File: "combined.tar.gz", Size: 2048, TaskCount: len(taskIDs)}, nil
}

func TestCloudDownloadCommand_CreateTask(t *testing.T) {
	ios, _, out, errOut := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudDownload(factory, ios, nil, nil)

	cmd.SetArgs([]string{"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	outStr := out.String()
	errStr := errOut.String()
	if errStr != "" {
		t.Logf("stderr: %s", errStr)
	}
	if !strings.Contains(outStr, "task-1") {
		t.Errorf("expected task ID in output, got: %s", outStr)
	}
}

func TestCloudDownloadCommand_MultipleURLs(t *testing.T) {
	ios, _, out, _ := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudDownload(factory, ios, nil, nil)

	cmd.SetArgs([]string{"https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "task-1") || !strings.Contains(out.String(), "task-2") {
		t.Errorf("expected both tasks in output, got: %s", out.String())
	}
}

func TestCloudDownloadCommand_NoURLs(t *testing.T) {
	ios, _, _, _ := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudDownload(factory, ios, nil, nil)

	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty URLs")
	}
}

func TestCloudListCommand(t *testing.T) {
	ios, _, out, _ := cli.BufferedIOStreams()
	mock := &mockCloudService{
		listCloudTasksFn: func(ctx context.Context, status string) ([]client.CloudTask, error) {
			return []client.CloudTask{
				{ID: "task-1", URL: "https://example.com/a.zip", Status: "completed"},
				{ID: "task-2", URL: "https://example.com/b.zip", Status: "downloading"},
			}, nil
		},
	}
	factory := clientfactory.NewMock(mock, nil)
	cmd := NewCmdCloudList(factory, ios, nil)

	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "task-1") {
		t.Errorf("expected task-1 in output, got: %s", out.String())
	}
}

func TestCloudListCommand_Empty(t *testing.T) {
	ios, _, out, _ := cli.BufferedIOStreams()
	mock := &mockCloudService{
		listCloudTasksFn: func(ctx context.Context, status string) ([]client.CloudTask, error) {
			return []client.CloudTask{}, nil
		},
	}
	factory := clientfactory.NewMock(mock, nil)
	cmd := NewCmdCloudList(factory, ios, nil)

	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "暂无") {
		t.Errorf("expected '暂无' message, got: %s", out.String())
	}
}

func TestCloudCancelCommand(t *testing.T) {
	ios, _, _, _ := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudCancel(factory, ios, nil)

	cmd.SetArgs([]string{"task-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudArchiveCommand_SingleTask(t *testing.T) {
	ios, _, out, _ := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudArchive(factory, ios, nil)

	cmd.SetArgs([]string{"task-1", "--name", "my-archive"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "archive.tar.gz") {
		t.Errorf("expected archive name in output, got: %s", out.String())
	}
}

func TestCloudArchiveCommand_MultipleTasks(t *testing.T) {
	ios, _, out, _ := cli.BufferedIOStreams()
	factory := clientfactory.NewMock(&mockCloudService{}, nil)
	cmd := NewCmdCloudArchive(factory, ios, nil)

	cmd.SetArgs([]string{"task-1", "task-2"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "combined.tar.gz") {
		t.Errorf("expected combined archive in output, got: %s", out.String())
	}
}
```

注意：需要在 `cloud_download_test.go` 的 import 中添加 `"fmt"` 和 `"strings"`。

- [ ] **步骤 2：运行测试验证通过**

```bash
go test -count=1 -v ./cmd/sclient/... -run TestCloudDownload\|TestCloudList\|TestCloudCancel\|TestCloudArchive
```

预期：所有测试 PASS

- [ ] **步骤 3：Commit**

```bash
git add cmd/sclient/cloud_download_test.go
git commit -m "test: add cloud download/list/cancel/archive command tests"
```

---

### 任务 12：全量测试验证

- [ ] **步骤 1：运行所有单元测试**

```bash
go test -count=1 -race ./internal/... ./pkg/... ./cmd/... 2>&1 | tail -30
```

预期：所有测试 PASS（已知的 E2E 测试先不在此运行）

- [ ] **步骤 2：运行 E2E 测试**

```bash
cd test && go test -count=1 -run TestE2E_SclientCLI -v -timeout 120s 2>&1
```

预期：PASS

- [ ] **步骤 3：运行所有 E2E 测试**

```bash
cd test && go test -count=1 -v -timeout 300s 2>&1 | tail -50
```

预期：所有 E2E 测试 PASS

- [ ] **步骤 4：运行 lint**

```bash
golangci-lint run ./pkg/... ./cmd/... 2>&1 | tail -20
```

预期：无 lint 错误

- [ ] **步骤 5：运行 go vet**

```bash
go vet ./pkg/... ./cmd/...
```

预期：无 vet 错误

---

### 任务 13：最终审查与提交

- [ ] **步骤 1：审查所有变更**

```bash
git diff --stat
git log --oneline -15
```

确认所有变更文件都在预期范围内。

- [ ] **步骤 2：创建分支**

```bash
git checkout -b feat/cloud-archive-workflow
```

- [ ] **步骤 3：请求代码审查**

使用 `requesting-code-review` 技能进行审查。

- [ ] **步骤 4：修复审查发现的缺陷**

如有发现，逐条修复。

- [ ] **步骤 5：创建 PR**

```bash
gh pr create --title "feat: cloud download archive workflow" --body "...
```

---