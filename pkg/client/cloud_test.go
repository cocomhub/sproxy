// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cloudTestServer 返回一个模拟 cloud download handler 的测试服务器。
func cloudTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	mux := http.NewServeMux()

	// POST /api/cloud/download
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

	// POST /api/cloud/download/batch
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

	// GET /api/cloud/tasks
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

	// GET /api/cloud/tasks/{id}
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
		json.NewEncoder(w).Encode(ArchiveResult{Success: true, File: "archive.tar.gz", Size: 1024, Checksum: "abc123", TaskCount: 1})
	})

	// POST /api/cloud/archive
	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ArchiveResult{Success: true, File: "combined.tar.gz", Size: 2048, Checksum: "def456", TaskCount: 2})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

// TestCloudDownload_CreateTask 测试创建单任务。
func TestCloudDownload_CreateTask(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	task, err := c.CloudDownload(t.Context(), "https://example.com/file.zip")
	if err != nil {
		t.Fatalf("CloudDownload: %v", err)
	}
	if task.ID != "test-task-1" {
		t.Fatalf("want task ID test-task-1, got %q", task.ID)
	}
	if task.URL != "https://example.com/file.zip" {
		t.Fatalf("want URL https://example.com/file.zip, got %q", task.URL)
	}
	if task.Status != "pending" {
		t.Fatalf("want status pending, got %q", task.Status)
	}
	if task.Filename == "" {
		t.Fatal("expected non-empty filename")
	}
}

// TestCloudDownload_CreateTaskWithFilename 测试带文件名选项创建单任务。
func TestCloudDownload_CreateTaskWithFilename(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	task, err := c.CloudDownload(t.Context(), "https://example.com/file.zip", WithCloudDownloadFilename("myfile.zip"))
	if err != nil {
		t.Fatalf("CloudDownload with filename: %v", err)
	}
	if task.Filename != "myfile.zip" {
		t.Fatalf("want filename myfile.zip, got %q", task.Filename)
	}
	if task.ID != "test-task-1" {
		t.Fatalf("want task ID test-task-1, got %q", task.ID)
	}
}

// TestCloudDownload_EmptyURL 测试空 URL 返回错误。
func TestCloudDownload_EmptyURL(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.CloudDownload(t.Context(), "")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

// TestCloudDownload_Batch 测试批量创建任务。
func TestCloudDownload_Batch(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	urls := []string{
		"https://example.com/a.zip",
		"https://example.com/b.zip",
	}
	tasks, err := c.CloudDownloadBatch(t.Context(), urls)
	if err != nil {
		t.Fatalf("CloudDownloadBatch: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" {
		t.Fatalf("want first task ID task-1, got %q", tasks[0].ID)
	}
	if tasks[1].ID != "task-2" {
		t.Fatalf("want second task ID task-2, got %q", tasks[1].ID)
	}
	if tasks[0].URL != "https://example.com/a.zip" {
		t.Fatalf("want URL https://example.com/a.zip, got %q", tasks[0].URL)
	}
}

// TestCloudDownload_ListTasks 测试列举任务。
func TestCloudDownload_ListTasks(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	tasks, err := c.ListCloudTasks(t.Context(), "")
	if err != nil {
		t.Fatalf("ListCloudTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" {
		t.Fatalf("want first task ID task-1, got %q", tasks[0].ID)
	}
	if tasks[1].ID != "task-2" {
		t.Fatalf("want second task ID task-2, got %q", tasks[1].ID)
	}
}

// TestCloudDownload_ListTasksFiltered 测试按状态过滤任务列表。
func TestCloudDownload_ListTasksFiltered(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	// 过滤 completed 状态
	tasks, err := c.ListCloudTasks(t.Context(), "completed")
	if err != nil {
		t.Fatalf("ListCloudTasks with status=completed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want 1 completed task, got %d", len(tasks))
	}
	if tasks[0].ID != "task-1" {
		t.Fatalf("want task ID task-1, got %q", tasks[0].ID)
	}
	if tasks[0].Status != "completed" {
		t.Fatalf("want status completed, got %q", tasks[0].Status)
	}
}

// TestCloudDownload_GetTask 测试查询单个任务。
func TestCloudDownload_GetTask(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	task, err := c.GetCloudTask(t.Context(), "task-1")
	if err != nil {
		t.Fatalf("GetCloudTask: %v", err)
	}
	if task.ID != "task-1" {
		t.Fatalf("want task ID task-1, got %q", task.ID)
	}
	if task.URL != "https://example.com/file.zip" {
		t.Fatalf("want URL https://example.com/file.zip, got %q", task.URL)
	}
	if task.Status != "completed" {
		t.Fatalf("want status completed, got %q", task.Status)
	}
}

// TestCloudDownload_GetTaskNotFound 测试查询不存在的任务。
func TestCloudDownload_GetTaskNotFound(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.GetCloudTask(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for notfound task")
	}
}

// TestCloudDownload_CancelTask 测试取消任务。
func TestCloudDownload_CancelTask(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.CancelCloudTask(t.Context(), "task-1"); err != nil {
		t.Fatalf("CancelCloudTask: %v", err)
	}
}

// TestCloudDownload_CancelTaskNotFound 测试取消不存在的任务。
func TestCloudDownload_CancelTaskNotFound(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.CancelCloudTask(t.Context(), "notfound"); err == nil {
		t.Fatal("expected error for cancelling notfound task")
	}
}

// TestCloudDownload_DeleteTask 测试删除任务。
func TestCloudDownload_DeleteTask(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.DeleteCloudTask(t.Context(), "task-1"); err != nil {
		t.Fatalf("DeleteCloudTask: %v", err)
	}
}

// TestCloudDownload_DeleteTaskNotFound 测试删除不存在的任务。
func TestCloudDownload_DeleteTaskNotFound(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.DeleteCloudTask(t.Context(), "notfound"); err == nil {
		t.Fatal("expected error for deleting notfound task")
	}
}

// TestCloudArchive_ArchiveTask 测试单任务归档。
func TestCloudArchive_ArchiveTask(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	result, err := c.ArchiveCloudTask(t.Context(), "task-1", "myarchive.tar.gz")
	if err != nil {
		t.Fatalf("ArchiveCloudTask: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success=true")
	}
	if result.File != "archive.tar.gz" {
		t.Fatalf("want file archive.tar.gz, got %q", result.File)
	}
	if result.Size != 1024 {
		t.Fatalf("want size 1024, got %d", result.Size)
	}
	if result.Checksum != "abc123" {
		t.Fatalf("want checksum abc123, got %q", result.Checksum)
	}
	if result.TaskCount != 1 {
		t.Fatalf("want TaskCount 1, got %d", result.TaskCount)
	}
}

// TestCloudArchive_ArchiveTaskNotFound 测试归档不存在的任务。
func TestCloudArchive_ArchiveTaskNotFound(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.ArchiveCloudTask(t.Context(), "notfound", "archive.tar.gz")
	if err == nil {
		t.Fatal("expected error for archiving notfound task")
	}
}

// TestCloudArchive_ArchiveTasks 测试批量归档。
func TestCloudArchive_ArchiveTasks(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	taskIDs := []string{"task-1", "task-2"}
	result, err := c.ArchiveCloudTasks(t.Context(), taskIDs, "combined.tar.gz")
	if err != nil {
		t.Fatalf("ArchiveCloudTasks: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success=true")
	}
	if result.File != "combined.tar.gz" {
		t.Fatalf("want file combined.tar.gz, got %q", result.File)
	}
	if result.Size != 2048 {
		t.Fatalf("want size 2048, got %d", result.Size)
	}
	if result.Checksum != "def456" {
		t.Fatalf("want checksum def456, got %q", result.Checksum)
	}
	if result.TaskCount != 2 {
		t.Fatalf("want TaskCount 2, got %d", result.TaskCount)
	}
}

// ---- CloudDownloadOption functions ----

func TestWithCloudDownloadMaxBatchURLs(t *testing.T) {
	o := &cloudDownloadOptions{}
	WithCloudDownloadMaxBatchURLs(50)(o)
	if o.maxBatchURLs != 50 {
		t.Errorf("maxBatchURLs = %d, want 50", o.maxBatchURLs)
	}
}

func TestWithCloudDownloadMaxBatchURLs_Zero(t *testing.T) {
	o := &cloudDownloadOptions{maxBatchURLs: 30}
	WithCloudDownloadMaxBatchURLs(0)(o)
	if o.maxBatchURLs != 30 {
		t.Errorf("maxBatchURLs should remain unchanged, got %d", o.maxBatchURLs)
	}
}

// ---- CloudDownload URL validation ----

func TestCloudDownload_InvalidURL(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	// 无效 URL 转义序列
	_, err := c.CloudDownload(t.Context(), "https://example.com/%zz")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestCloudDownload_Batch_EmptyList(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.CloudDownloadBatch(t.Context(), []string{})
	if err == nil {
		t.Fatal("expected error for empty URL list")
	}
}

func TestCloudDownload_Batch_ExceedsLimit(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	// 101 个 URL，超过默认 100 上限
	urls := make([]string, 101)
	for i := range 101 {
		urls[i] = "https://example.com/file"
	}
	_, err := c.CloudDownloadBatch(t.Context(), urls)
	if err == nil {
		t.Fatal("expected error for exceeding batch limit")
	}
}

func TestCloudDownload_Batch_CustomLimit(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	// 用自定义上限 5 来限制
	urls := make([]string, 6)
	for i := range 6 {
		urls[i] = "https://example.com/file"
	}
	_, err := c.CloudDownloadBatch(t.Context(), urls, WithCloudDownloadMaxBatchURLs(5))
	if err == nil {
		t.Fatal("expected error for exceeding custom batch limit of 5")
	}
}

func TestCloudDownload_Batch_UnderCustomLimit(t *testing.T) {
	t.Parallel()
	ts, _ := cloudTestServer(t)
	c := NewFileClient(ts.URL)

	urls := make([]string, 3)
	for i := range 3 {
		urls[i] = "https://example.com/file"
	}
	tasks, err := c.CloudDownloadBatch(t.Context(), urls, WithCloudDownloadMaxBatchURLs(5))
	if err != nil {
		t.Fatalf("expected success for 3 URLs under limit of 5: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
}
