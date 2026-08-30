// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// syncTestServer 返回模拟 /api/sync/tasks 端点的测试服务器。
// 对齐服务端 sync_handler.go 的 JSON 契约：
//   - POST /api/sync/tasks → 201 直接返回 SyncTask（去重复用 200）
//   - GET  /api/sync/tasks → {success, tasks:[SyncTaskMeta]}
//   - GET  /api/sync/tasks/{id} → SyncTask / 404 {error}
//   - POST /api/sync/tasks/{id}/cancel → 200 {status:"cancelled"} / 404 {error}
//   - DELETE /api/sync/tasks/{id} → 200 {status:"deleted"} / 404 {error}
func syncTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req SyncTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.Direction != "push" && req.Direction != "pull" {
			http.Error(w, `{"error":"invalid direction"}`, http.StatusBadRequest)
			return
		}
		if req.Remote == "nope" {
			http.Error(w, `{"error":"remote \"nope\" 未配置"}`, http.StatusBadRequest)
			return
		}
		if req.Direction == "pull" && req.Remote == "full" {
			http.Error(w, `{"error":"storage quota exceeded"}`, http.StatusInsufficientStorage)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(SyncTask{
			ID:             "sync-test-1",
			Direction:      req.Direction,
			Remote:         req.Remote,
			Src:            req.Src,
			Dst:            req.Dst,
			Recursive:      req.Recursive,
			Include:        req.Include,
			Exclude:        req.Exclude,
			ConflictPolicy: req.ConflictPolicy,
			SyncEmptyDirs:  req.SyncEmptyDirs,
			FollowSymlinks: req.FollowSymlinks,
			Status:         SyncStatusPending,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
			ExpiresAt:      time.Now().Add(24 * time.Hour),
		})
	})

	mux.HandleFunc("GET /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		tasks := []SyncTaskMeta{
			{ID: "sync-1", Direction: "push", Remote: "r1", Status: "completed", FilesTotal: 2, FilesDone: 2, BytesTotal: 100, BytesDone: 100},
			{ID: "sync-2", Direction: "pull", Remote: "r2", Status: "syncing"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "tasks": tasks})
	})

	mux.HandleFunc("GET /api/sync/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncTask{
			ID: id, Direction: "push", Remote: "r1", Status: SyncStatusCompleted,
			FilesTotal: 2, FilesDone: 2, BytesTotal: 100, BytesDone: 100,
		})
	})

	mux.HandleFunc("POST /api/sync/tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
	})

	mux.HandleFunc("DELETE /api/sync/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

// TestSyncCreateTask_RequestPathAndBody 验证 POST 请求的路径/方法与 body JSON 字段。
func TestSyncCreateTask_RequestPathAndBody(t *testing.T) {
	t.Parallel()
	var gotPath, gotMethod string
	var gotReq SyncTaskRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(SyncTask{ID: "sync-captured", Direction: gotReq.Direction, Remote: gotReq.Remote, Status: "pending"})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	_, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{
		Direction: "push", Remote: "r1", Src: "a/b.txt", Dst: "x/y.txt",
		Recursive: true, Include: []string{"*.go"}, Exclude: []string{"*.tmp"},
		ConflictPolicy: "lww", SyncEmptyDirs: true, FollowSymlinks: true,
	})
	if err != nil {
		t.Fatalf("CreateSyncTask: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/sync/tasks" {
		t.Fatalf("want POST /api/sync/tasks, got %s %s", gotMethod, gotPath)
	}
	if gotReq.Direction != "push" || gotReq.Remote != "r1" || gotReq.Src != "a/b.txt" || gotReq.Dst != "x/y.txt" {
		t.Fatalf("request body fields mismatch: %+v", gotReq)
	}
	if !gotReq.Recursive || !gotReq.SyncEmptyDirs || !gotReq.FollowSymlinks {
		t.Fatalf("bool flags not serialized: %+v", gotReq)
	}
	if gotReq.ConflictPolicy != "lww" {
		t.Fatalf("want conflict_policy lww, got %q", gotReq.ConflictPolicy)
	}
	if len(gotReq.Include) != 1 || gotReq.Include[0] != "*.go" {
		t.Fatalf("include not serialized: %v", gotReq.Include)
	}
	if len(gotReq.Exclude) != 1 || gotReq.Exclude[0] != "*.tmp" {
		t.Fatalf("exclude not serialized: %v", gotReq.Exclude)
	}
}

// TestSyncCreateTask 测试创建同步任务（201 响应解析）。
func TestSyncCreateTask(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	task, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{
		Direction: "push", Remote: "r1", Src: "dir/file.txt", Dst: "dst",
		ConflictPolicy: "overwrite",
	})
	if err != nil {
		t.Fatalf("CreateSyncTask: %v", err)
	}
	if task.ID != "sync-test-1" {
		t.Fatalf("want task ID sync-test-1, got %q", task.ID)
	}
	if task.Direction != "push" || task.Remote != "r1" {
		t.Fatalf("task fields mismatch: %+v", task)
	}
	if task.Src != "dir/file.txt" || task.Dst != "dst" {
		t.Fatalf("task paths mismatch: %+v", task)
	}
	if task.Status != SyncStatusPending {
		t.Fatalf("want status pending, got %q", task.Status)
	}
}

// TestSyncCreateTask_InvalidDirection 测试服务端 400 错误信息透出。
func TestSyncCreateTask_InvalidDirection(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{Direction: "sideways", Remote: "r1"})
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
	if !strings.Contains(err.Error(), "invalid direction") {
		t.Fatalf("expected server error message surfaced, got: %v", err)
	}
}

// TestSyncCreateTask_RemoteNotConfigured 测试 400（remote 未配置）。
func TestSyncCreateTask_RemoteNotConfigured(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{Direction: "push", Remote: "nope"})
	if err == nil {
		t.Fatal("expected error for unconfigured remote")
	}
	if !strings.Contains(err.Error(), "未配置") {
		t.Fatalf("expected remote-not-configured message, got: %v", err)
	}
}

// TestSyncCreateTask_StorageFull 测试 507 → ErrStorageFull 哨兵错误。
func TestSyncCreateTask_StorageFull(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{Direction: "pull", Remote: "full"})
	if err == nil {
		t.Fatal("expected error for storage full")
	}
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("expected ErrStorageFull, got: %v", err)
	}
}

// TestSyncGetTask 测试查询单个任务。
func TestSyncGetTask(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	task, err := c.GetSyncTask(t.Context(), "sync-abc")
	if err != nil {
		t.Fatalf("GetSyncTask: %v", err)
	}
	if task.ID != "sync-abc" {
		t.Fatalf("want task ID sync-abc, got %q", task.ID)
	}
	if task.Status != SyncStatusCompleted {
		t.Fatalf("want status completed, got %q", task.Status)
	}
}

// TestSyncGetTask_NotFound 测试 404 → ErrNotFound。
func TestSyncGetTask_NotFound(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.GetSyncTask(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for notfound task")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// TestSyncGetTask_EmptyID 测试空 ID 返回错误。
func TestSyncGetTask_EmptyID(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	_, err := c.GetSyncTask(t.Context(), "")
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
}

// TestSyncListTasks 测试列表（{success, tasks} 容器解析）。
func TestSyncListTasks(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	tasks, err := c.ListSyncTasks(t.Context())
	if err != nil {
		t.Fatalf("ListSyncTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "sync-1" {
		t.Fatalf("want first task ID sync-1, got %q", tasks[0].ID)
	}
	if tasks[0].Status != "completed" || tasks[0].FilesTotal != 2 {
		t.Fatalf("first task fields mismatch: %+v", tasks[0])
	}
}

// TestSyncCancelTask 测试取消任务。
func TestSyncCancelTask(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.CancelSyncTask(t.Context(), "sync-1"); err != nil {
		t.Fatalf("CancelSyncTask: %v", err)
	}
}

// TestSyncCancelTask_NotFound 测试取消不存在的任务（404 → ErrNotFound）。
func TestSyncCancelTask_NotFound(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	err := c.CancelSyncTask(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for cancel notfound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// TestSyncDeleteTask 测试删除任务。
func TestSyncDeleteTask(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	if err := c.DeleteSyncTask(t.Context(), "sync-1"); err != nil {
		t.Fatalf("DeleteSyncTask: %v", err)
	}
}

// TestSyncDeleteTask_NotFound 测试删除不存在的任务（404 → ErrNotFound）。
func TestSyncDeleteTask_NotFound(t *testing.T) {
	t.Parallel()
	ts, _ := syncTestServer(t)
	c := NewFileClient(ts.URL)

	err := c.DeleteSyncTask(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for delete notfound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// TestSyncCancelTask_EmptyID / TestSyncDeleteTask_EmptyID 验证空 ID 快速报错（审查 R-1）。
func TestSyncCancelTask_EmptyID(t *testing.T) {
	t.Parallel()
	c := NewFileClient("http://127.0.0.1:1")
	if err := c.CancelSyncTask(t.Context(), ""); err == nil {
		t.Fatal("expected error for empty cancel id")
	}
}

func TestSyncDeleteTask_EmptyID(t *testing.T) {
	t.Parallel()
	c := NewFileClient("http://127.0.0.1:1")
	if err := c.DeleteSyncTask(t.Context(), ""); err == nil {
		t.Fatal("expected error for empty delete id")
	}
}

// TestSyncListTasks_Empty 验证空列表 {success:true, tasks:[]} 解析（审查 R-1）。
func TestSyncListTasks_Empty(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "tasks": []SyncTaskMeta{}})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	tasks, err := c.ListSyncTasks(t.Context())
	if err != nil {
		t.Fatalf("ListSyncTasks error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("expected empty list, got %d", len(tasks))
	}
}

// TestSyncCreateTask_DedupReuse200 验证服务端去重复用返回 200（同样解析 SyncTask，
// 审查 R-1）。doJSON 对 200/201 均解析 body。
func TestSyncCreateTask_DedupReuse200(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // 去重复用既有活跃任务
		json.NewEncoder(w).Encode(SyncTask{
			ID: "sync-existing", Direction: "push", Remote: "r1",
			Src: "a", Dst: "b", Status: SyncStatusSyncing,
			CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	task, err := c.CreateSyncTask(t.Context(), SyncTaskRequest{Direction: "push", Remote: "r1", Src: "a", Dst: "b"})
	if err != nil {
		t.Fatalf("CreateSyncTask (200 dedup) error: %v", err)
	}
	if task.ID != "sync-existing" || task.Status != SyncStatusSyncing {
		t.Fatalf("expected dedup task, got: %+v", task)
	}
}
