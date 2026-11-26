// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestShortChecksumForLog_NoPanic 锁定日志截断不得越界（空/短 checksum 曾会 panic）。
func TestShortChecksumForLog_NoPanic(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                             "-",
		"abc":                          "abc",
		"0123456789abcdef":             "0123456789abcdef",
		"0123456789abcdef0123456789ab": "0123456789abcdef...",
	}
	for in, want := range cases {
		if got := shortChecksumForLog(in); got != want {
			t.Fatalf("shortChecksumForLog(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCleanupTaskCloud_FailurePersisted 锁定「自动清理失败也会落盘」：
// 否则重启后 CleanupStatus 显示 cleaned，掩盖真实失败（2026-11-27 审查）。
func TestCleanupTaskCloud_FailurePersisted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	// Save=false + DownloadLocal=false 会触发自动清理；但创建期要求"有产出"，故给 transfer 目标
	// （清理逻辑只依赖 Save/DownloadLocal，与 transfer 无关）。
	lfs := syncpkg.NewLocalFS(root, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-CLEANUP-1", "c.bin", 1024, "",
		TaskParams{Save: false, Transfer: &TransferSpec{Volume: "v", Path: "c/out.bin"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	dir := mgr.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	// 用「目录」占住 destPath ⇒ os.Remove 失败（非空目录）
	dest := filepath.Join(dir, task.Filename)
	if merr := os.MkdirAll(filepath.Join(dest, "x"), 0o755); merr != nil {
		t.Fatalf("造占位: %v", merr)
	}
	mgr.cleanupTaskCloud(task, dest)
	if got := mgr.tasks[task.ID].CleanupStatus; got != "failed" {
		t.Fatalf("清理失败应记 failed，got %q", got)
	}
	mgr.flushDirty()
	raw, rerr := os.ReadFile(filepath.Join(mgr.PersistDirFor(task.Owner), task.ID+".json"))
	if rerr != nil {
		t.Fatalf("读持久化: %v", rerr)
	}
	if !strings.Contains(string(raw), `"cleanup_status":"failed"`) {
		t.Fatalf("失败状态应落盘，got %s", string(raw))
	}
	_ = time.Now()
}
