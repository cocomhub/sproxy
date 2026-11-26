// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestKeepPartialOnFail 锁定「瞬态失败保留断点」判据（用户 2026-11-26 实测驱动）：
// 只有「下载过字节 + 瞬态错误 + 未显式关闭」才保留；完整性/磁盘满/未知一律删除。
func TestKeepPartialOnFail(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mgr, _ := newCloudTestManager(t, root, capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger()), actionsTestConfig())
	m := mgr
	task, err := m.CreateTask("url", "https://example.com/s/EXAMPLE-KEEP-JUDGE", "k.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	dir := m.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	if merr := mkdirAllAndWritePartial(dir, task.Filename); merr != nil {
		t.Fatalf("造断点: %v", merr)
	}
	m.mu.Lock()
	task.Downloaded = 8 << 20
	m.mu.Unlock()
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
	} {
		if m.keepPartialOnFail(task, msg) {
			t.Fatalf("断点不可信/没空间时不应保留: %q", msg)
		}
	}
	// 未知错误**保留**（更可能是瞬态；删掉会白丢可续进度）——2026-11-26 复审修正。
	if !m.keepPartialOnFail(task, "something totally unknown") {
		t.Fatal("未知错误应保留断点")
	}
	// 磁盘上没有断点 ⇒ 不保留（用另一任务验证）
	empty, cerr := m.CreateTask("url", "https://example.com/s/EXAMPLE-KEEP-EMPTY", "e.bin", 1024, "", TaskParams{Save: true})
	if cerr != nil {
		t.Fatalf("CreateTask: %v", cerr)
	}
	if m.keepPartialOnFail(empty, "source unreachable") {
		t.Fatal("磁盘无断点时应返回 false")
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

// TestSettleArtifacts_DeletionClearsPartialDiagnostics 锁定「产物删除后诊断清零」：
// 否则动作提示会误报「保留已下载部分，从断点继续」（完整性失败/磁盘满走删除分支）。
func TestSettleArtifacts_DeletionClearsPartialDiagnostics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-CLEAR-1", "c.bin", 4096, "",
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
	task.Diagnostics = &TaskDiagnostics{PartialSize: 2048, PartialPath: dir}
	mgr.mu.Unlock()

	// 完整性失败 ⇒ 删除分支
	mgr.settleArtifactsOnFail(task, "integrity mismatch: gcid", nil)
	if task.Diagnostics.PartialSize != 0 || task.Diagnostics.PartialPath != "" {
		t.Fatalf("删除后诊断应清零，got size=%d path=%q", task.Diagnostics.PartialSize, task.Diagnostics.PartialPath)
	}
	if fileExists(filepath.Join(dir, task.Filename+".partial")) {
		t.Fatal("完整性失败应删除 .partial")
	}
	// 提示必须诚实：无断点 ⇒ 从零重下
	if got := retryDownloadHint(task); !strings.Contains(got, "无可续断点") {
		t.Fatalf("hint = %q, want 从零重下提示", got)
	}
}

// TestDiscardPartialOnFail_ReleasesQuota 锁定「非瞬态失败删除断点后账本归零」：
// 否则磁盘已空、账本仍占 ⇒ 长期虚占配额（用户 2026-11-26 生产关注点）。
func TestDiscardPartialOnFail_ReleasesQuota(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-DISCARD-1", "d.bin", 4096, "",
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
	// CreateTask 已按 totalSize 预留 4096（已知大小）——这里保持不动，验证删除后预留被释放。
	task.Diagnostics = &TaskDiagnostics{PartialSize: 2048, PartialPath: dir}
	mgr.mu.Unlock()
	if got := sm.UsageByCategory()[capacity.CategoryCloud]; got != 4096 {
		t.Fatalf("前置：账本应为 4096，got %d", got)
	}

	mgr.discardPartialOnFail(task)

	if got := sm.UsageByCategory()[capacity.CategoryCloud]; got != 0 {
		t.Fatalf("删除断点后全局账本 = %d, want 0（否则长期虚占）", got)
	}
	if task.ReservedSize != 0 {
		t.Fatalf("ReservedSize = %d, want 0", task.ReservedSize)
	}
	if task.Diagnostics.PartialSize != 0 || task.Diagnostics.PartialPath != "" {
		t.Fatalf("诊断应清零，got %+v", task.Diagnostics)
	}
	if fileExists(dir) {
		t.Fatal("任务目录应被删除")
	}
}
