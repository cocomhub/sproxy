// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestKeepPartialOnFail 锁定「瞬态失败保留断点」判据（用户 2026-11-26 实测驱动）：
// 只有「下载过字节 + 瞬态错误 + 未显式关闭」才保留；完整性/磁盘满/未知一律删除。
func TestKeepPartialOnFail(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{config: &CloudDownloadConfig{}}
	task := &CloudTask{Downloaded: 8 << 20}
	for _, msg := range []string{
		"source unreachable: dial tcp: i/o timeout",
		"throttled: HTTP 429",
		"auth: 401 unauthorized",
	} {
		if !m.keepPartialOnFail(task, msg) {
			t.Fatalf("瞬态失败应保留断点: %q", msg)
		}
	}
	for _, msg := range []string{
		"integrity mismatch: gcid",
		"disk_full: no space left on device",
		"something totally unknown",
	} {
		if m.keepPartialOnFail(task, msg) {
			t.Fatalf("非瞬态失败不应保留断点: %q", msg)
		}
	}
	// 没有下载过字节 ⇒ 无断点可留
	if m.keepPartialOnFail(&CloudTask{Downloaded: 0}, "source unreachable") {
		t.Fatal("未下载字节时不应保留")
	}
	// 显式关闭 ⇒ 一律不保留
	m.config.DiscardPartialOnTransientFailure = true
	if m.keepPartialOnFail(task, "source unreachable") {
		t.Fatal("显式关闭后不得保留")
	}
}

// TestSettleArtifacts_KeepsPartialOnTransient 锁定行为（不只是判据）：
// 瞬态失败后任务目录里的 .partial 必须还在，retry_download 才真能续传。
func TestSettleArtifacts_KeepsPartialOnTransient(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-KEEP-1", "k.bin", 4096, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	dir := mgr.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	if merr := mkdirAllAndWritePartial(dir, task.Filename); merr != nil {
		t.Fatalf("造断点: %v", merr)
	}
	mgr.mu.Lock()
	task.Downloaded = 2048
	mgr.mu.Unlock()

	mgr.settleArtifactsOnFail(task, "source unreachable: i/o timeout", nil)

	if !fileExists(filepath.Join(dir, task.Filename+".partial")) {
		t.Fatal("瞬态失败后 .partial 必须保留（否则 retry_download 无法续传）")
	}
	if task.Diagnostics == nil || task.Diagnostics.PartialSize == 0 {
		t.Fatalf("应记录 partial 诊断: %+v", task.Diagnostics)
	}
}

// TestReuseTerminal_RequiresSameFilename 锁定「同 URL 不同文件名不得复用」
// （2026-11-26 实测：提交 e2e3.bin 却复用了 e2e2.bin 的任务）。
func TestReuseTerminal_RequiresSameFilename(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	const url = "https://example.com/s/EXAMPLE-NAME-1"
	first, err := mgr.CreateTask("url", url, "a.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	mgr.tasks[first.ID].Status = "failed"
	mgr.tasks[first.ID].UpdatedAt = time.Now()
	mgr.mu.Unlock()

	second, err := mgr.CreateTask("url", url, "b.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask(2): %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("文件名不同不应复用旧任务（会静默沿用旧落盘名）")
	}
}
