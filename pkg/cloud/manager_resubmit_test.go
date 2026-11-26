// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func resubmitTestConfig() *CloudDownloadConfig {
	return &CloudDownloadConfig{
		MaxConcurrent: 1,
		TaskTTL:       time.Hour,
		FailedTaskTTL: 24 * time.Hour,
		IdleTimeout:   time.Minute,
		SyncThreshold: 1 << 20,
	}
}

// TestCreateTask_ResubmitRevivesFailedTask 锁定「同一 URL 重提命中失败任务 ⇒ 复活并保留进度」
// （用户 2026-11-26：慢源重试不得从 0 开始）。
func TestCreateTask_ResubmitRevivesFailedTask(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, resubmitTestConfig())
	params := TaskParams{Save: true}
	const url = "https://example.com/s/EXAMPLE-RESUBMIT-1"

	task, err := mgr.CreateTask("url", url, "a.mp4", 1024, "", params)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// 模拟「下载失败但已下 1234 字节」（保留进度）。
	mgr.mu.Lock()
	stored := mgr.tasks[task.ID]
	stored.Status = "failed"
	stored.Error = "boom"
	stored.Downloaded = 1234
	stored.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	again, err := mgr.CreateTask("url", url, "a.mp4", 1024, "", params)
	if err != nil {
		t.Fatalf("CreateTask(resubmit): %v", err)
	}
	if again.ID != task.ID {
		t.Fatalf("重提应复用失败任务 %s，实际新建 %s（会从 0 重下）", task.ID, again.ID)
	}
	if again.Status != "pending" {
		t.Fatalf("复活后状态应为 pending，实际 %q", again.Status)
	}
	if again.Downloaded != 1234 {
		t.Fatalf("复活后应保留进度 1234，实际 %d", again.Downloaded)
	}
	if again.Error != "" {
		t.Fatalf("复活后应清空错误，实际 %q", again.Error)
	}
}

// TestCreateTask_ResubmitTerminal_SemanticsDifferCreatesNew 锁定「语义不同不复用」：
// transfer 目标不同的同 URL 请求必须新建任务（否则本次意图被静默吞掉）。
func TestCreateTask_ResubmitTerminal_SemanticsDifferCreatesNew(t *testing.T) {
	t.Parallel()
	lfs := syncpkg.NewLocalFS(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	root2 := t.TempDir()
	sm2 := capacity.NewStorageManager(filepath.Join(root2, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root2, sm2, resubmitTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	const url = "https://example.com/s/EXAMPLE-RESUBMIT-2"

	first, err := mgr.CreateTask("url", url, "a.mp4", 1024, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v1", Path: "x/a.mp4"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	mgr.tasks[first.ID].Status = "failed"
	mgr.tasks[first.ID].UpdatedAt = time.Now()
	mgr.mu.Unlock()

	second, err := mgr.CreateTask("url", url, "a.mp4", 1024, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v2", Path: "y/a.mp4"}})
	if err != nil {
		t.Fatalf("CreateTask(2): %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("transfer 目标不同却被复用（语义被吞）")
	}
}

// TestCreateTask_ResubmitCompletedIdempotent 锁定「completed 且目标存在 ⇒ 幂等短路」（不重复下载）。
func TestCreateTask_ResubmitCompletedIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lfs := syncpkg.NewLocalFS(root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sm3 := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm3, resubmitTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	const url = "https://example.com/s/EXAMPLE-RESUBMIT-3"
	params := TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v", Path: "x/a.mp4"}}

	task, err := mgr.CreateTask("url", url, "a.mp4", 1024, "", params)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if merr := os.MkdirAll(filepath.Join(root, "x"), 0o755); merr != nil {
		t.Fatalf("mkdir: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(root, "x", "a.mp4"), []byte("ok"), 0o644); werr != nil {
		t.Fatalf("write target: %v", werr)
	}
	mgr.mu.Lock()
	mgr.tasks[task.ID].Status = "completed"
	mgr.tasks[task.ID].UpdatedAt = time.Now()
	mgr.mu.Unlock()

	again, err := mgr.CreateTask("url", url, "a.mp4", 1024, "", params)
	if err != nil {
		t.Fatalf("CreateTask(resubmit): %v", err)
	}
	if again.ID != task.ID || again.Status != "completed" {
		t.Fatalf("目标存在时应幂等返回 completed 任务，实际 id=%s status=%s", again.ID, again.Status)
	}
}

// TestResubmitRevive_ClearsStaleChunks 锁定「复活任务清空上一轮分块状态」：
// HTTP 下载器不报分块 ⇒ 不清会让 UI 永久显示旧的"已完成块"。
func TestResubmitRevive_ClearsStaleChunks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, resubmitTestConfig())
	const url = "https://example.com/s/EXAMPLE-REVIVE-CHUNKS"
	task, err := mgr.CreateTask("url", url, "rc.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	stored := mgr.tasks[task.ID]
	stored.Status = "failed"
	stored.Chunks = []downloader.ChunkState{{Index: 0, Phase: "done", Length: 1024, Done: 1024}}
	stored.Progress = []ProgressSample{{Downloaded: 512, Total: 1024}}
	stored.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	again, err := mgr.CreateTask("url", url, "rc.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask(resubmit): %v", err)
	}
	if again.ID != task.ID {
		t.Fatalf("应复活原任务，实际新建 %s", again.ID)
	}
	if len(again.Chunks) != 0 {
		t.Fatalf("复活后应清空分块状态，got %d", len(again.Chunks))
	}
	if len(again.Progress) == 0 {
		t.Fatal("进度历史应保留（跨重试曲线）")
	}
}
