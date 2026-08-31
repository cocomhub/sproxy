// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// newMockGroupChainServer 创建覆盖组链式操作完整 API 的 mock 服务端。
// groupStatusFn 每次查询组详情时调用，返回组状态与子任务列表。
func newMockGroupChainServer(t *testing.T, dir string, groupStatusFn func(poll int) (string, []CloudTask)) *httptest.Server {
	t.Helper()
	archiveDir := filepath.Join(dir, ".__cloud_archives__")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	var poll atomic.Int32
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string                `json:"name"`
			URLs []cloudfilename.Entry `json:"urls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		group := CloudGroup{
			ID:         "group-test",
			Name:       req.Name,
			Status:     "pending",
			TotalTasks: len(req.URLs),
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		for i := range req.URLs {
			group.TaskIDs = append(group.TaskIDs, fmt.Sprintf("gt-%d", i+1))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(group)
	})

	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id != "group-test" {
			http.Error(w, `{"error":"group not found"}`, http.StatusNotFound)
			return
		}
		status, tasks := groupStatusFn(int(poll.Add(1)))
		group := &CloudGroup{
			ID:         id,
			Name:       "test-group",
			Status:     status,
			TaskIDs:    []string{"gt-1", "gt-2"},
			TotalTasks: 2,
			Completed:  0,
			Failed:     0,
			CreatedAt:  time.Now(),
			UpdatedAt:  time.Now(),
		}
		for _, t := range tasks {
			if t.Status == TaskStatusCompleted {
				group.Completed++
			}
			if t.Status == TaskStatusFailed {
				group.Failed++
			}
		}
		detail := map[string]any{"group": group, "tasks": tasks}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
	})

	mux.HandleFunc("POST /api/cloud/groups/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		archivePath := filepath.Join(archiveDir, "group-archive.tar.gz")
		if err := os.WriteFile(archivePath, []byte("group-archive-content"), 0644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(CloudArchiveResult{
			Success: true,
			Message: "ok",
			File:    filepath.ToSlash(filepath.Join(".__cloud_archives__", "group-archive.tar.gz")),
			Size:    int64(len("group-archive-content")),
		})
	})

	mux.HandleFunc("DELETE /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	})

	// Stat endpoint for chunked download
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		archiveFile := resolveMockDownloadFile(dir, r)
		os.MkdirAll(filepath.Dir(archiveFile), 0755)
		if _, err := os.Stat(archiveFile); err != nil {
			os.WriteFile(archiveFile, []byte("group-archive-content"), 0644)
		}
		data, err := os.ReadFile(archiveFile)
		if err != nil {
			t.Error("ReadFile:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sum := testutil.SHA256Hex(data)
		info, err := os.Stat(archiveFile)
		if err != nil {
			t.Error("Stat:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
		w.Header().Set("X-File-Checksum", sum)
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))
		w.WriteHeader(http.StatusOK)
	})

	// Chunk download endpoint
	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		archiveFile := resolveMockDownloadFile(dir, r)
		data, err := os.ReadFile(archiveFile)
		if err != nil {
			t.Error("ReadFile:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(data)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// completedGroupTasks 返回两个已完成的子任务。
func completedGroupTasks() []CloudTask {
	return []CloudTask{
		{ID: "gt-1", Status: TaskStatusCompleted},
		{ID: "gt-2", Status: TaskStatusCompleted},
	}
}

func TestCloudDownloadGroupChain_ImplementsChainRunner(t *testing.T) {
	t.Parallel()
	var _ ChainRunner = (*CloudDownloadGroupChain)(nil)
}

func TestCloudDownloadGroupChain_New(t *testing.T) {
	t.Parallel()
	client := NewFileClient("http://127.0.0.1:9999")
	entries := []cloudfilename.Entry{
		{URL: "https://example.com/file1"},
		{URL: "https://example.com/file2"},
	}
	opts := defaultChainOptions()

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "test-group-archive", t.TempDir(), opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	if chain.ID() == "" {
		t.Fatal("expected non-empty chain ID")
	}
	if chain.Status() != StatusRunning {
		t.Errorf("expected status=running, got %s", chain.Status())
	}
	if chain.TotalTasks != 2 {
		t.Errorf("expected total=2, got %d", chain.TotalTasks)
	}
	if chain.GroupName != "g1" {
		t.Errorf("expected group name g1, got %s", chain.GroupName)
	}
	if chain.ArchiveName != "test-group-archive" {
		t.Errorf("expected archive name, got %s", chain.ArchiveName)
	}
}

func TestCloudDownloadGroupChain_State(t *testing.T) {
	t.Parallel()
	client := NewFileClient("http://127.0.0.1:9999")
	entries := []cloudfilename.Entry{{URL: "https://example.com/file"}}
	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", t.TempDir(), defaultChainOptions())
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}
	chain.CurrentPhase = PhaseWaiting
	chain.GroupID = "group-1"
	chain.Completed = 1

	state := chain.State()
	if state["type"] != TypeCloudDownloadGroup {
		t.Errorf("expected type=%s, got %v", TypeCloudDownloadGroup, state["type"])
	}
	if state["phase"] != PhaseWaiting {
		t.Errorf("expected phase=waiting, got %v", state["phase"])
	}
	if state["group_name"] != "g1" {
		t.Errorf("expected group_name=g1, got %v", state["group_name"])
	}
	if state["group_id"] != "group-1" {
		t.Errorf("expected group_id=group-1, got %v", state["group_id"])
	}
}

func TestCloudDownloadGroupChain_Restore(t *testing.T) {
	t.Parallel()
	state := map[string]any{
		"type":          TypeCloudDownloadGroup,
		"chain_id":      "group-chain-123",
		"phase":         PhaseWaiting,
		"status":        StatusRunning,
		"group_name":    "g1",
		"group_id":      "group-1",
		"archive_name":  "my-archive",
		"local_dir":     t.TempDir(),
		"keep_files":    false,
		"total_tasks":   2.0,
		"completed":     1.0,
		"failed":        0.0,
		"cancelled":     0.0,
		"created_at":    time.Now(),
		"updated_at":    time.Now(),
		"poll_interval": int64(3000000000),
		"timeout":       int64(60000000000),
	}

	chain := &CloudDownloadGroupChain{}
	if err := chain.Restore(state); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	if chain.ChainID != "group-chain-123" {
		t.Errorf("expected chain-id group-chain-123, got %s", chain.ChainID)
	}
	if chain.CurrentPhase != PhaseWaiting {
		t.Errorf("expected phase=waiting, got %s", chain.Phase())
	}
	if chain.GroupName != "g1" {
		t.Errorf("expected group name g1, got %s", chain.GroupName)
	}
	if chain.GroupID != "group-1" {
		t.Errorf("expected group id group-1, got %s", chain.GroupID)
	}
}

func TestCloudDownloadGroupChain_FullRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := newMockGroupChainServer(t, dir, func(poll int) (string, []CloudTask) {
		return "completed", completedGroupTasks()
	})

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{
		{URL: "https://example.com/file1"},
		{URL: "https://example.com/file2"},
	}
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond
	opts.timeout = 10 * time.Second

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "group-archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	var phases []string
	reportFn := func(ctx context.Context, info ProgressInfo) {
		phases = append(phases, info.Phase)
	}

	err = chain.Run(t.Context(), reportFn)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if chain.CurrentPhase != PhaseCompleted {
		t.Errorf("expected phase=completed, got %s", chain.Phase())
	}
	if chain.CurStatus != StatusCompleted {
		t.Errorf("expected status=completed, got %s", chain.Status())
	}
	if chain.GroupID != "group-test" {
		t.Errorf("expected group ID group-test, got %s", chain.GroupID)
	}
	if chain.Completed != 2 {
		t.Errorf("expected completed=2, got %d", chain.Completed)
	}
	if chain.LocalPath == "" {
		t.Fatal("expected local path to be set")
	}
	if _, err := os.Stat(chain.LocalPath); err != nil {
		t.Errorf("expected local file to exist: %v", err)
	}

	expectedPhases := []string{PhaseSubmitting, PhaseWaiting, PhaseArchiving, PhaseDownloading, PhaseCleaning}
	if len(phases) < len(expectedPhases) {
		t.Fatalf("expected at least %d phases, got %d: %v", len(expectedPhases), len(phases), phases)
	}
	for i, p := range expectedPhases {
		if phases[i] != p {
			t.Errorf("phase[%d] expected %s, got %s", i, p, phases[i])
		}
	}
}

func TestCloudDownloadGroupChain_KeepFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := newMockGroupChainServer(t, dir, func(poll int) (string, []CloudTask) {
		return "completed", completedGroupTasks()
	})

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond
	opts.timeout = 10 * time.Second
	opts.keepFiles = true

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "keep-archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	var phases []string
	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {
		phases = append(phases, info.Phase)
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	for _, p := range phases {
		if p == PhaseCleaning {
			t.Fatal("expected no cleaning phase when keepFiles=true")
		}
	}
}

func TestCloudDownloadGroupChain_SubmitError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", t.TempDir(), opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error for submit failure")
	}
}

func TestCloudDownloadGroupChain_ArchiveError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudGroup{ID: "group-test", Status: "pending", TotalTasks: 1})
	})
	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"group": &CloudGroup{ID: "group-test", Status: "completed", TotalTasks: 1, Completed: 1},
			"tasks": []CloudTask{{ID: "gt-1", Status: TaskStatusCompleted}},
		})
	})
	// 归档失败
	mux.HandleFunc("POST /api/cloud/groups/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudArchiveResult{Success: false, Message: "archive failed"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", t.TempDir(), opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error for archive failure")
	}
	if !errors.Is(err, ErrArchiveFailed) {
		t.Errorf("expected ErrArchiveFailed, got %v", err)
	}
}

func TestCloudDownloadGroupChain_WaitFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := newMockGroupChainServer(t, dir, func(poll int) (string, []CloudTask) {
		return "partial", []CloudTask{
			{ID: "gt-1", Status: TaskStatusCompleted},
			{ID: "gt-2", Status: TaskStatusFailed, Error: "connection refused"},
		}
	})

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{
		{URL: "https://example.com/file1"},
		{URL: "https://example.com/file2"},
	}
	opts := defaultChainOptions()
	opts.pollInterval = 50 * time.Millisecond
	opts.timeout = 5 * time.Second

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error when a task fails in the group")
	}
	if !strings.Contains(err.Error(), "失败") {
		t.Errorf("expected error mentioning failure, got: %v", err)
	}
	// 失败时状态应为 failed，且不进入 archiving
	if chain.CurrentPhase != PhaseFailed {
		t.Errorf("expected phase=failed, got %s", chain.Phase())
	}
}

func TestCloudDownloadGroupChain_RunNoClient(t *testing.T) {
	t.Parallel()
	chain := &CloudDownloadGroupChain{}
	err := chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error for nil client")
	}
	if !errors.Is(err, ErrClientNil) {
		t.Errorf("expected ErrClientNil, got %v", err)
	}
}

func TestCloudDownloadGroupChain_CleanupRemoteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, ".__cloud_archives__")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudGroup{ID: "group-test", Status: "pending", TotalTasks: 1})
	})
	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"group": &CloudGroup{ID: "group-test", Status: "completed", TotalTasks: 1, Completed: 1},
			"tasks": []CloudTask{{ID: "gt-1", Status: TaskStatusCompleted}},
		})
	})
	mux.HandleFunc("POST /api/cloud/groups/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		archivePath := filepath.Join(archiveDir, "cleanup-archive.tar.gz")
		os.WriteFile(archivePath, []byte("cleanup"), 0644)
		json.NewEncoder(w).Encode(CloudArchiveResult{
			Success: true, File: filepath.ToSlash(filepath.Join(".__cloud_archives__", "cleanup-archive.tar.gz")), Size: 7,
		})
	})
	// 删除组失败：cleanupGroup 应容忍（Run 仍成功）
	mux.HandleFunc("DELETE /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 50 * time.Millisecond
	opts.timeout = 5 * time.Second

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "cleanup-archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	// 空 GroupID 时 cleanupGroup 是 no-op（返回 nil）
	if err := chain.cleanupGroup(t.Context()); err != nil {
		t.Fatalf("expected no error when group ID empty, got: %v", err)
	}
	// 设置 group ID 后调用：删除失败时返回 error
	chain.GroupID = "group-test"
	if err := chain.cleanupGroup(t.Context()); err == nil {
		t.Fatal("expected cleanupGroup error when delete fails")
	}
}

func TestCloudDownloadGroupChain_WaitTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudGroup{ID: "group-timeout", Status: "pending", TotalTasks: 1})
	})
	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"group": &CloudGroup{ID: "group-timeout", Status: "downloading", TotalTasks: 1},
			"tasks": []CloudTask{{ID: "gt-1", Status: "downloading"}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 50 * time.Millisecond
	opts.timeout = 200 * time.Millisecond

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout/deadline error, got: %v", err)
	}
}

func TestCloudDownloadGroupChain_WaitCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudGroup{ID: "group-cancel", Status: "pending", TotalTasks: 1})
	})
	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"group": &CloudGroup{ID: "group-cancel", Status: "cancelled", TotalTasks: 1, Completed: 0, Failed: 0},
			"tasks": []CloudTask{{ID: "gt-1", Status: TaskStatusCancelled}},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 50 * time.Millisecond
	opts.timeout = 5 * time.Second

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error when tasks are cancelled")
	}
	if !strings.Contains(err.Error(), "取消") {
		t.Errorf("expected error mentioning cancel, got: %v", err)
	}
}

func TestCloudDownloadGroupChain_WaitNilGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudGroup{ID: "group-nil", Status: "pending", TotalTasks: 1})
	})
	mux.HandleFunc("GET /api/cloud/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"group": nil,
			"tasks": []CloudTask{},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	entries := []cloudfilename.Entry{{URL: "https://example.com/file1"}}
	opts := defaultChainOptions()
	opts.pollInterval = 50 * time.Millisecond
	opts.timeout = 5 * time.Second

	chain, err := NewCloudDownloadGroupChain(client, "g1", entries, "archive", dir, opts)
	if err != nil {
		t.Fatalf("NewCloudDownloadGroupChain failed: %v", err)
	}

	err = chain.Run(t.Context(), func(ctx context.Context, info ProgressInfo) {})
	if err == nil {
		t.Fatal("expected error when group is nil")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected error mentioning group not found, got: %v", err)
	}
}

// TestCloudDownloadGroupChain_ResumeRestoresOptions 验证 ResumeChain 能正确恢复
// CloudDownloadGroupChain 的轮询间隔/超时/keepFiles 选项（此前 ResumeChain 只识别
// CloudDownloadChain，组链式操作恢复后会退回默认值 3s/30m/false）。
//
// 测试方法：直接在 KVStore 中保存一个中间状态（PhaseWaiting），模拟链式操作被中断后
// 恢复的场景，验证恢复后的 runner 能正确取回非默认选项并继续运行。
func TestCloudDownloadGroupChain_ResumeRestoresOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ts := newMockGroupChainServer(t, dir, func(poll int) (string, []CloudTask) {
		return "completed", completedGroupTasks()
	})

	store := NewMemoryKVStore()
	client := NewFileClient(ts.URL, WithKVStore(store))

	// 模拟一个组链式操作在 PhaseWaiting 阶段中断后的持久化状态，使用非默认选项
	phaseState := map[string]any{
		"type":          TypeCloudDownloadGroup,
		"chain_id":      "group-chain-resume-options",
		"phase":         PhaseWaiting,
		"status":        StatusRunning,
		"group_name":    "resume-group",
		"group_id":      "group-test",
		"entries":       nil,
		"archive_name":  "group-resume-archive",
		"local_dir":     dir,
		"keep_files":    true,
		"total_tasks":   2.0,
		"completed":     0.0,
		"failed":        0.0,
		"cancelled":     0.0,
		"created_at":    time.Now(),
		"updated_at":    time.Now(),
		"poll_interval": int64(100 * time.Millisecond),
		"timeout":       int64(10 * time.Second),
	}
	if err := store.Save(t.Context(), "chain-group-chain-resume-options", phaseState); err != nil {
		t.Fatal(err)
	}

	// 通过 ResumeChain 恢复
	resumed, err := client.ResumeChain(t.Context(), "group-chain-resume-options")
	if err != nil {
		t.Fatalf("ResumeChain failed: %v", err)
	}
	if !resumed.KeepFiles() {
		t.Error("expected keep_files=true restored from group chain")
	}
	if resumed.GetExtraValue("group_id") != "group-test" {
		t.Errorf("expected group_id group-test, got %v", resumed.GetExtraValue("group_id"))
	}
	// 验证返回的 extra 字段包含 local_path
	if resumed.LocalPath() == "" {
		t.Error("expected non-empty local_path after resume")
	}
}
