// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func actionsTestConfig() *CloudDownloadConfig {
	return &CloudDownloadConfig{
		MaxConcurrent: 1, TaskTTL: time.Hour, FailedTaskTTL: 24 * time.Hour,
		IdleTimeout: time.Minute, SyncThreshold: 1 << 20,
	}
}

// TestParseTaskAction 锁定动作名解析（未知动作明确报错，便于 CLI/HTTP 快速失败）。
func TestParseTaskAction(t *testing.T) {
	t.Parallel()
	for _, a := range []string{"retry_download", "retry_transfer", "rotate_source", "discard_partial"} {
		if got, err := ParseTaskAction(a); err != nil || string(got) != a {
			t.Fatalf("ParseTaskAction(%q) = %q, %v", a, got, err)
		}
	}
	if _, err := ParseTaskAction("solve"); err == nil {
		t.Fatal("未知动作应报错")
	}
}

// TestPlanTaskActions_Availability 锁定「动作可用性由服务端判定」：
// 下载未完成 ⇒ 仅重做转存禁用；无备选源 ⇒ 换源禁用；非失败态 ⇒ 无动作。
func TestPlanTaskActions_Availability(t *testing.T) {
	t.Parallel()
	plan := PlanTaskActions(&CloudTask{Status: "failed"})
	if len(plan) != 4 {
		t.Fatalf("失败任务应有 4 个动作，got %d", len(plan))
	}
	byAction := map[TaskAction]TaskActionPlan{}
	for _, p := range plan {
		byAction[p.Action] = p
	}
	if !byAction[TaskActionRetryTransfer].Disabled {
		t.Fatal("下载未完成时「仅重做转存」应禁用")
	}
	if !byAction[TaskActionRotateSource].Disabled {
		t.Fatal("无备选源时「换源重试」应禁用")
	}
	withArtifact := PlanTaskActions(&CloudTask{
		Status: "failed", DownloadDone: true, Transfer: &TransferSpec{Volume: "v", Path: "a/b"},
		FallbackSources: []string{"u1", "u2"},
	})
	for _, p := range withArtifact {
		if p.Disabled {
			t.Fatalf("动作 %s 不应禁用（reason=%s）", p.Action, p.Reason)
		}
	}
	if got := PlanTaskActions(&CloudTask{Status: "completed"}); len(got) != 0 {
		t.Fatalf("非失败态不应有动作，got %d", len(got))
	}
}

// TestActionResumeOptions_Preconditions 锁定前置条件（不可行动作必须报错，不得静默降级）。
func TestActionResumeOptions_Preconditions(t *testing.T) {
	t.Parallel()
	if _, err := actionResumeOptions(TaskActionRetryTransfer, &CloudTask{Status: "failed"}); err == nil {
		t.Fatal("下载未完成时 retry_transfer 应报错")
	}
	if _, err := actionResumeOptions(TaskActionRetryTransfer, &CloudTask{Status: "failed", DownloadDone: true}); err == nil {
		t.Fatal("无转存目标时 retry_transfer 应报错")
	}
	if _, err := actionResumeOptions(TaskActionRotateSource, &CloudTask{Status: "failed", FallbackSources: []string{"u1"}}); err == nil {
		t.Fatal("无备选源时 rotate_source 应报错")
	}
	opts, err := actionResumeOptions(TaskActionRetryTransfer, &CloudTask{
		Status: "failed", DownloadDone: true, Transfer: &TransferSpec{Volume: "v", Path: "p"},
	})
	if err != nil || !opts.keepDownloadDone {
		t.Fatalf("retry_transfer 应保留产物标记，got %+v, %v", opts, err)
	}
}

// TestApplyTaskAction_RetryTransferKeepsArtifact 锁定「仅重做转存不重下」：
// DownloadDone 保留 ⇒ executeDownload 走产物复用分支。
func TestApplyTaskAction_RetryTransferKeepsArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	lfs := syncpkg.NewLocalFS(root, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-ACTION-1", "a.mp4", 1024, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v", Path: "a/a.mp4"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.Error = "transfer: boom"
	task.DownloadDone = true
	task.DownloadedSize = 1024
	task.DownloadedChecksum = "abc"
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()
	// 「仅重做转存」要求磁盘产物仍在（2026-11-26 前置校验）：先造产物。
	dir := mgr.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	if merr := os.MkdirAll(dir, 0o755); merr != nil {
		t.Fatalf("mkdir: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(dir, task.Filename), make([]byte, 1024), 0o644); werr != nil {
		t.Fatalf("write artifact: %v", werr)
	}

	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRetryTransfer, ""); err != nil {
		t.Fatalf("ApplyTaskAction: %v", err)
	}
	mgr.mu.RLock()
	got := *mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if !got.DownloadDone || got.DownloadedSize != 1024 || got.DownloadedChecksum != "abc" {
		t.Fatalf("仅重做转存必须保留产物标记，got done=%v size=%d ck=%q",
			got.DownloadDone, got.DownloadedSize, got.DownloadedChecksum)
	}
}

// TestApplyTaskAction_RetryDownloadClearsArtifact 锁定「退回下载阶段续传」：清产物标记但保留 Downloaded 进度。
func TestApplyTaskAction_RetryDownloadClearsArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-ACTION-2", "b.mp4", 1024, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.DownloadDone = true
	task.DownloadedSize = 512
	task.DownloadedChecksum = "def"
	task.Downloaded = 512
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRetryDownload, ""); err != nil {
		t.Fatalf("ApplyTaskAction: %v", err)
	}
	mgr.mu.RLock()
	got := *mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if got.DownloadDone || got.DownloadedChecksum != "" {
		t.Fatalf("退回下载应清产物标记，got done=%v ck=%q", got.DownloadDone, got.DownloadedChecksum)
	}
	if got.Status != "pending" {
		t.Fatalf("动作提交后应为 pending，got %q", got.Status)
	}
}

// TestApplyTaskAction_RotateSource 锁定「换源重下」：URL 切到下一个备选源。
func TestApplyTaskAction_RotateSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	const u1, u2 = "https://example.com/s/EXAMPLE-ACTION-3", "https://example.com/s/EXAMPLE-ACTION-4"
	task, err := mgr.CreateTask("url", u1, "c.mp4", 1024, "",
		TaskParams{Save: true, FallbackSources: []string{u1, u2}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRotateSource, ""); err != nil {
		t.Fatalf("ApplyTaskAction: %v", err)
	}
	mgr.mu.RLock()
	got := *mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if got.URL != u2 || got.SourceIndex != 1 {
		t.Fatalf("应切换到备选源 %s，got url=%s idx=%d", u2, got.URL, got.SourceIndex)
	}
}

// TestApplyTaskAction_RejectsNonResumable 锁定「非失败态不得执行动作」（错误必须可见）。
func TestApplyTaskAction_RejectsNonResumable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-ACTION-5", "d.mp4", 1024, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "completed"
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()
	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRetryDownload, ""); err == nil {
		t.Fatal("completed（非 damaged）不应允许 retry_download")
	}
	if _, err := mgr.ApplyTaskAction("no-such-task", TaskActionRetryDownload, ""); err == nil {
		t.Fatal("不存在的任务应报错")
	}
}

// TestPlanTaskActions_Damaged 锁定「completed+damaged 有修复入口」（定点修复优先）。
func TestPlanTaskActions_Damaged(t *testing.T) {
	t.Parallel()
	plan := PlanTaskActions(&CloudTask{Status: "completed", IntegrityStatus: "damaged"})
	if len(plan) == 0 {
		t.Fatal("damaged 任务应提供修复动作")
	}
	if plan[0].Action != TaskActionRetryDownload || plan[0].Label != "修复重下" {
		t.Fatalf("首个动作应为「修复重下」的 retry_download，got %+v", plan[0])
	}
	if got := PlanTaskActions(&CloudTask{Status: "completed"}); len(got) != 0 {
		t.Fatalf("正常 completed 不应有动作，got %d", len(got))
	}
}

// TestVerifyActionPreconditions_RetryTransferRequiresArtifact 锁定「产物缺失时不得静默重下」：
// 仅重做转存必须先确认磁盘产物存在，否则明确报错（避免 executeDownload 静默回落整份下载）。
func TestVerifyActionPreconditions_RetryTransferRequiresArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-PRECOND-1", "e.mp4", 1024, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.DownloadDone = true
	mgr.mu.Unlock()
	if err := mgr.verifyActionPreconditions(TaskActionRetryTransfer, task); err == nil {
		t.Fatal("产物不存在时应报错（不能静默重下）")
	}
	// 造出产物后应通过。
	dir := mgr.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	if merr := os.MkdirAll(dir, 0o755); merr != nil {
		t.Fatalf("mkdir: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(dir, task.Filename), []byte("x"), 0o644); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if err := mgr.verifyActionPreconditions(TaskActionRetryTransfer, task); err != nil {
		t.Fatalf("产物存在时应通过: %v", err)
	}
}

// TestTaskActionCounters 锁定动作计数（按 action|result 分标签）。
func TestTaskActionCounters(t *testing.T) {
	t.Parallel()
	var c TaskActionCounters
	c.Inc("retry_download", "success")
	c.Inc("retry_download", "success")
	c.Inc("rotate_source", "error")
	c.Inc("", "success") // 空动作忽略
	snap := c.Snapshot()
	if snap["retry_download|success"] != 2 || snap["rotate_source|error"] != 1 || len(snap) != 2 {
		t.Fatalf("计数异常: %+v", snap)
	}
}
