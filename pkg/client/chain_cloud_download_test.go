// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudDownloadChain_ImplementsChainRunner(t *testing.T) {
	t.Parallel()
	var _ ChainRunner = (*CloudDownloadChain)(nil)
}

func TestCloudDownloadChain_New(t *testing.T) {
	t.Parallel()
	client := NewFileClient("http://127.0.0.1:9999")
	opts := defaultChainOptions()

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file1", "http://example.com/file2"}, "test-archive", t.TempDir(), opts)

	if chain.ID() == "" {
		t.Fatal("expected non-empty chain ID")
	}
	if chain.Phase() != "" {
		t.Errorf("expected empty phase, got %s", chain.Phase())
	}
	if chain.Status() != StatusRunning {
		t.Errorf("expected status=running, got %s", chain.Status())
	}
	if chain.Total != 2 {
		t.Errorf("expected total=2, got %d", chain.Total)
	}
	if len(chain.URLs) != 2 {
		t.Errorf("expected 2 URLs, got %d", len(chain.URLs))
	}
	if chain.ArchiveName != "test-archive" {
		t.Errorf("expected archive name test-archive, got %s", chain.ArchiveName)
	}
	if chain.LocalDir == "" {
		t.Errorf("expected non-empty local dir")
	}
}

func TestCloudDownloadChain_State(t *testing.T) {
	t.Parallel()
	client := NewFileClient("http://127.0.0.1:9999")
	opts := defaultChainOptions()

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file"}, "archive", "/tmp/out", opts)
	chain.CurrentPhase = PhaseWaiting
	chain.TaskIDs = []string{"task-1", "task-2"}
	chain.Completed = 1
	chain.Failed = 0

	state := chain.State()

	if state["type"] != "cloud_download" {
		t.Errorf("expected type=cloud_download, got %v", state["type"])
	}
	if state["phase"] != PhaseWaiting {
		t.Errorf("expected phase=waiting, got %v", state["phase"])
	}
	if state["status"] != StatusRunning {
		t.Errorf("expected status=running, got %v", state["status"])
	}
	if len(state["urls"].([]string)) != 1 {
		t.Errorf("expected 1 URL, got %d", len(state["urls"].([]string)))
	}
	if len(state["task_ids"].([]string)) != 2 {
		t.Errorf("expected 2 task IDs, got %d", len(state["task_ids"].([]string)))
	}
}

func TestCloudDownloadChain_Restore(t *testing.T) {
	t.Parallel()
	state := map[string]any{
		"type":         "cloud_download",
		"chain_id":     "chain-123",
		"phase":        PhaseWaiting,
		"status":       StatusRunning,
		"urls":         []any{"http://example.com/file"},
		"task_ids":     []any{"task-1", "task-2"},
		"archive_name": "my-archive",
		"local_dir":    "/tmp/out",
		"keep_files":   false,
		"completed":    1.0,
		"failed":       0.0,
		"total":        2.0,
	}

	chain := &CloudDownloadChain{}
	if err := chain.Restore(state); err != nil {
		t.Fatalf("Restore failed: %v", err)
	}

	if chain.ChainID != "chain-123" {
		t.Errorf("expected chain-123, got %s", chain.ChainID)
	}
	if chain.CurrentPhase != PhaseWaiting {
		t.Errorf("expected phase=waiting, got %s", chain.Phase())
	}
	if chain.Completed != 1 {
		t.Errorf("expected completed=1, got %d", chain.Completed)
	}
	if chain.Total != 2 {
		t.Errorf("expected total=2, got %d", chain.Total)
	}
	if len(chain.TaskIDs) != 2 {
		t.Errorf("expected 2 task IDs, got %d", len(chain.TaskIDs))
	}
}

func TestCloudDownloadChain_FullRun(t *testing.T) {
	t.Parallel()
	ts, dir := newMockCloudServer(t)
	defer ts.Close()

	client := NewFileClient(ts.URL)
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond
	opts.timeout = 10 * time.Second

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file1", "http://example.com/file2"}, "test-archive", dir, opts)

	var phases []string
	reportFn := func(ctx context.Context, phase string, msg string, current, total int) {
		phases = append(phases, phase)
	}

	err := chain.Run(context.Background(), reportFn)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if chain.CurrentPhase != PhaseCompleted {
		t.Errorf("expected phase=completed, got %s", chain.Phase())
	}
	if chain.CurStatus != StatusCompleted {
		t.Errorf("expected status=completed, got %s", chain.Status())
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

	// 验证 phase 顺序（PhaseCompleted 是最终状态，不通过 reportFn 报告）
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

func TestCloudDownloadChain_KeepFiles(t *testing.T) {
	t.Parallel()
	ts, dir := newMockCloudServer(t)
	defer ts.Close()

	client := NewFileClient(ts.URL)
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond
	opts.timeout = 10 * time.Second
	opts.keepFiles = true

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file1"}, "keep-archive", dir, opts)

	var phases []string
	reportFn := func(ctx context.Context, phase string, msg string, current, total int) {
		phases = append(phases, phase)
	}

	err := chain.Run(context.Background(), reportFn)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// 验证 keepFiles=true 时没有 cleaning phase
	for _, p := range phases {
		if p == PhaseCleaning {
			t.Fatal("expected no cleaning phase when keepFiles=true")
		}
	}
}

func TestCloudDownloadChain_SubmitError(t *testing.T) {
	t.Parallel()
	// 服务端返回 500
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file1"}, "archive", "/tmp", opts)

	err := chain.Run(context.Background(), func(ctx context.Context, phase string, msg string, current, total int) {})
	if err == nil {
		t.Fatal("expected error for submit failure")
	}
}

func TestCloudDownloadChain_ArchiveError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()

	var taskIDCounter atomic.Int64

	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		tasks := []CloudTask{{ID: fmt.Sprintf("task-%d", taskIDCounter.Add(1)), Status: "pending"}}
		json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
	})

	mux.HandleFunc("GET /api/cloud/tasks/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudTask{ID: "task-1", Status: "completed"})
	})

	// 归档失败
	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ArchiveResult{Success: false, Message: "archive failed"})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	opts := defaultChainOptions()
	opts.pollInterval = 100 * time.Millisecond

	chain := NewCloudDownloadChain(client, []string{"http://example.com/file1"}, "archive", "/tmp", opts)

	err := chain.Run(context.Background(), func(ctx context.Context, phase string, msg string, current, total int) {})
	if err == nil {
		t.Fatal("expected error for archive failure")
	}
	if !strings.Contains(err.Error(), "打包归档失败") {
		t.Errorf("expected archive error message, got %v", err)
	}
}

func TestIsStorageFullError(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"storage full", true},
		{"STORAGE FULL", true},
		{"Storage Full", true},
		{"507", true},
		{"Insufficient Storage", true},
		{"insufficient storage", true},
		{"INSUFFICIENT STORAGE", true},
		{"disk quota exceeded", true},
		{"no space left on device", true},
		{"disk full", false},
		{"", false},
		{"network error", false},
	}
	for _, tt := range tests {
		got := isStorageFullError(tt.msg)
		if got != tt.want {
			t.Errorf("isStorageFullError(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}

func TestCloudDownloadChain_ResumeMidway(t *testing.T) {
	t.Parallel()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	// 保存一个中间状态（PhaseWaiting）
	state := map[string]any{
		"type":         "cloud_download",
		"chain_id":     "chain-resume",
		"phase":        PhaseWaiting,
		"status":       StatusRunning,
		"urls":         []any{"http://example.com/file1"},
		"task_ids":     []any{"task-1", "task-2"},
		"archive_name": "resume-archive",
		"local_dir":    t.TempDir(),
		"keep_files":   false,
		"completed":    0.0,
		"failed":       0.0,
		"total":        2.0,
		"created_at":   time.Now(),
		"updated_at":   time.Now(),
	}
	if err := store.Save(context.Background(), "chain:chain-resume", state); err != nil {
		t.Fatal(err)
	}

	// 恢复 runner
	runner, err := cm.Resume(context.Background(), "chain-resume")
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	if runner.Phase() != PhaseWaiting {
		t.Errorf("expected phase=waiting, got %s", runner.Phase())
	}
	state2 := runner.State()
	if state2["archive_name"] != "resume-archive" {
		t.Errorf("expected archive_name=resume-archive, got %v", state2["archive_name"])
	}
}

func TestCloudDownloadChain_ResumeAndRun(t *testing.T) {
	t.Parallel()
	ts, dir := newMockCloudServer(t)
	defer ts.Close()

	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	state := map[string]any{
		"type":         "cloud_download",
		"chain_id":     "chain-resume-run",
		"phase":        PhaseWaiting,
		"status":       StatusRunning,
		"urls":         []any{"http://example.com/file1"},
		"task_ids":     []any{"task-1", "task-2"},
		"archive_name": "resume-run-archive",
		"local_dir":    dir,
		"keep_files":   false,
		"completed":    0.0,
		"failed":       0.0,
		"total":        2.0,
		"created_at":   time.Now(),
		"updated_at":   time.Now(),
	}
	if err := store.Save(context.Background(), "chain:chain-resume-run", state); err != nil {
		t.Fatal(err)
	}

	runner, err := cm.Resume(context.Background(), "chain-resume-run")
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	cdc, ok := runner.(*CloudDownloadChain)
	if !ok {
		t.Fatal("expected CloudDownloadChain")
	}
	cdc.setClient(NewFileClient(ts.URL))
	cdc.setOptions(chainOptions{pollInterval: 100 * time.Millisecond, timeout: 10 * time.Second})

	var phases []string
	err = runner.Run(context.Background(), func(ctx context.Context, phase string, msg string, current, total int) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("Resumed Run failed: %v", err)
	}

	if runner.Phase() != PhaseCompleted {
		t.Errorf("expected phase=completed, got %s", runner.Phase())
	}
	if runner.Status() != StatusCompleted {
		t.Errorf("expected status=completed, got %s", runner.Status())
	}
	if len(phases) < 3 {
		t.Fatalf("expected at least 3 phases after resume, got %d: %v", len(phases), phases)
	}
}

func TestCloudDownloadChain_StorageFullRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "__cloud_archives__")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	var taskIDCounter atomic.Int64

	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		tasks := []CloudTask{
			{ID: fmt.Sprintf("task-%d", taskIDCounter.Add(1)), Status: "pending"},
			{ID: fmt.Sprintf("task-%d", taskIDCounter.Add(1)), Status: "pending"},
		}
		json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
	})

	mux.HandleFunc("POST /api/cloud/download", func(w http.ResponseWriter, r *http.Request) {
		task := CloudTask{ID: fmt.Sprintf("task-%d", taskIDCounter.Add(1)), Status: "pending"}
		json.NewEncoder(w).Encode(task)
	})

	var queryCount atomic.Int64
	mux.HandleFunc("GET /api/cloud/tasks/", func(w http.ResponseWriter, r *http.Request) {
		count := queryCount.Add(1)
		taskID := strings.TrimPrefix(r.URL.Path, "/api/cloud/tasks/")
		if count <= 3 {
			json.NewEncoder(w).Encode(CloudTask{
				ID:     taskID,
				Status: "failed",
				Error:  "storage full",
				URL:    "http://example.com/retry-file",
			})
		} else {
			json.NewEncoder(w).Encode(CloudTask{
				ID:     taskID,
				Status: "completed",
			})
		}
	})

	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		archivePath := filepath.Join(archiveDir, "retry-archive.tar.gz")
		os.WriteFile(archivePath, []byte("archive-content"), 0644)
		sum := sha256.Sum256([]byte("archive-content"))
		json.NewEncoder(w).Encode(ArchiveResult{
			Success:  true,
			Message:  "ok",
			File:     filepath.ToSlash(filepath.Join("__cloud_archives__", "retry-archive.tar.gz")),
			Size:     15,
			Checksum: hex.EncodeToString(sum[:]),
		})
	})

	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		filename := r.URL.Query().Get("filename")
		archiveFile := filepath.Join(dir, filepath.FromSlash(filename))
		os.MkdirAll(filepath.Dir(archiveFile), 0755)
		if _, err := os.Stat(archiveFile); err != nil {
			os.WriteFile(archiveFile, []byte("archive-content"), 0644)
		}
		data, _ := os.ReadFile(archiveFile)
		sum := sha256.Sum256(data)
		info, _ := os.Stat(archiveFile)
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		filename := r.URL.Query().Get("filename")
		archiveFile := filepath.Join(dir, filepath.FromSlash(filename))
		data, _ := os.ReadFile(archiveFile)
		w.Write(data)
	})
	mux.HandleFunc("DELETE /api/cloud/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := NewFileClient(ts.URL)
	opts := chainOptions{pollInterval: 50 * time.Millisecond, timeout: 10 * time.Second}

	chain := NewCloudDownloadChain(client, []string{"http://example.com/f1", "http://example.com/f2"}, "retry-archive", dir, opts)

	err := chain.Run(context.Background(), func(ctx context.Context, phase string, msg string, current, total int) {})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if chain.Phase() != PhaseCompleted {
		t.Errorf("expected phase=completed, got %s", chain.Phase())
	}
	if len(chain.TaskIDs) < 3 {
		t.Errorf("expected at least 3 task IDs after retry, got %d", len(chain.TaskIDs))
	}
}

// newMockCloudServer 创建带云端下载 API 完整 mock 的服务端。
func newMockCloudServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	mux := http.NewServeMux()

	archiveDir := filepath.Join(dir, "__cloud_archives__")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatal(err)
	}

	var taskIDCounter atomic.Int64

	mux.HandleFunc("POST /api/cloud/download/batch", func(w http.ResponseWriter, r *http.Request) {
		var tasks []CloudTask
		for range 2 {
			id := fmt.Sprintf("task-%d", taskIDCounter.Add(1))
			tasks = append(tasks, CloudTask{
				ID:     id,
				Status: "pending",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
	})

	mux.HandleFunc("POST /api/cloud/download", func(w http.ResponseWriter, r *http.Request) {
		id := fmt.Sprintf("task-%d", taskIDCounter.Add(1))
		json.NewEncoder(w).Encode(CloudTask{ID: id, Status: "pending"})
	})

	mux.HandleFunc("GET /api/cloud/tasks/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(CloudTask{
			ID:     strings.TrimPrefix(r.URL.Path, "/api/cloud/tasks/"),
			Status: "completed",
		})
	})

	mux.HandleFunc("POST /api/cloud/archive", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TaskIDs     []string `json:"task_ids"`
			ArchiveName string   `json:"archive_name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		archivePath := filepath.Join(archiveDir, req.ArchiveName+".tar.gz")
		os.WriteFile(archivePath, []byte("archive-content"), 0644)
		sum := sha256.Sum256([]byte("archive-content"))
		json.NewEncoder(w).Encode(ArchiveResult{
			Success:  true,
			Message:  "ok",
			File:     filepath.ToSlash(filepath.Join("__cloud_archives__", req.ArchiveName+".tar.gz")),
			Size:     15,
			Checksum: hex.EncodeToString(sum[:]),
		})
	})

	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		filename := r.URL.Query().Get("filename")
		archiveFile := filepath.Join(dir, filepath.FromSlash(filename))
		os.MkdirAll(filepath.Dir(archiveFile), 0755)
		if _, err := os.Stat(archiveFile); err != nil {
			os.WriteFile(archiveFile, []byte("archive-content"), 0644)
		}
		data, _ := os.ReadFile(archiveFile)
		sum := sha256.Sum256(data)
		info, _ := os.Stat(archiveFile)
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		filename := r.URL.Query().Get("filename")
		archiveFile := filepath.Join(dir, filepath.FromSlash(filename))
		data, _ := os.ReadFile(archiveFile)
		w.Write(data)
	})

	mux.HandleFunc("DELETE /api/cloud/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, dir
}
