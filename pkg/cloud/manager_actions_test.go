// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
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
	if len(plan) != 6 {
		t.Fatalf("失败任务应有 6 个动作（续传/仅转存/覆盖转存/换链接/换源/清残留），got %d", len(plan))
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
		Diagnostics:     &TaskDiagnostics{LastErrorClass: ErrClassTargetConflict}, // 覆盖转存仅冲突时可用
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
	if _, err := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryTransfer}, &CloudTask{Status: "failed"}); err == nil {
		t.Fatal("下载未完成时 retry_transfer 应报错")
	}
	if _, err := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryTransfer}, &CloudTask{Status: "failed", DownloadDone: true}); err == nil {
		t.Fatal("无转存目标时 retry_transfer 应报错")
	}
	if _, err := actionResumeOptions(TaskActionRequest{Action: TaskActionRotateSource}, &CloudTask{Status: "failed", FallbackSources: []string{"u1"}}); err == nil {
		t.Fatal("无备选源时 rotate_source 应报错")
	}
	opts, err := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryTransfer}, &CloudTask{
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
	// 动作提交后任务立即被 goroutine 接管：状态可能是 pending 或已进入 downloading
	// （CI 慢机实测竞态）——断言"已重新开始"而不是钉死中间态。
	if got.Status != "pending" && got.Status != "downloading" {
		t.Fatalf("动作提交后应重新开始（pending/downloading），got %q", got.Status)
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

// TestApplyTaskAction_ClearsChunkMatrixOnRedownload 锁定「重下清空旧块矩阵」：
// 否则 UI 会把上一轮已完成的块显示成当前进度（生产加固 2026-11-26）。
func TestApplyTaskAction_ClearsChunkMatrixOnRedownload(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-CHUNKS-1", "f.mp4", 1024, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.Chunks = []downloader.ChunkState{{Index: 0, Phase: "done", Length: 1024, Done: 1024}}
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRetryDownload, ""); err != nil {
		t.Fatalf("ApplyTaskAction: %v", err)
	}
	mgr.mu.RLock()
	got := len(mgr.tasks[task.ID].Chunks)
	mgr.mu.RUnlock()
	if got != 0 {
		t.Fatalf("重下应清空旧块矩阵，got %d 块", got)
	}
}

// TestCreateTask_RejectsInvalidTransferPath 锁定「转存路径创建期校验」：
// `../escape` 这类非法路径必须 400（2026-11-26 实测：此前返回 200，直到转存阶段才失败）。
func TestCreateTask_RejectsInvalidTransferPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	vol := volume.Volume{Name: "main", Type: "local",
		ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"": {}}}}
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig(), CloudManagerOptions{
		TransferFSFor: func(string) (syncpkg.FS, string) { return syncpkg.NewLocalFS(root, testLogger()), "local" },
		VolumeFor:     func(string) (volume.Volume, bool) { return vol, true },
	})
	for _, bad := range []string{"../escape.bin", "/abs.bin", "a/../../b.bin"} {
		if _, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-PATH-1", "x.bin", 10, "",
			TaskParams{Save: true, Transfer: &TransferSpec{Volume: "main", Path: bad}}); err == nil {
			t.Fatalf("非法转存路径 %q 应在创建期被拒", bad)
		}
	}
	if _, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-PATH-2", "x.bin", 10, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "main", Path: "ok/dir/x.bin"}}); err != nil {
		t.Fatalf("合法路径不应被拒: %v", err)
	}
}

// TestActionResumeOptions_OverwriteAndFromURL 锁定两个新动作的前置条件与语义。
func TestActionResumeOptions_OverwriteAndFromURL(t *testing.T) {
	t.Parallel()
	withArtifact := &CloudTask{Status: "failed", DownloadDone: true, Transfer: &TransferSpec{Volume: "v", Path: "p"}}
	opts, err := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryTransferOverwrite}, withArtifact)
	if err != nil || !opts.keepDownloadDone {
		t.Fatalf("覆盖重试应保留产物标记，got %+v, %v", opts, err)
	}
	if _, oerr := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryTransferOverwrite},
		&CloudTask{Status: "failed", DownloadDone: true}); oerr == nil {
		t.Fatal("无转存目标时应报错")
	}
	if _, uerr := actionResumeOptions(TaskActionRequest{Action: TaskActionRetryFromURL},
		&CloudTask{Status: "failed"}); uerr == nil {
		t.Fatal("缺 URL 时应报错")
	}
	opts, err = actionResumeOptions(TaskActionRequest{Action: TaskActionRetryFromURL, URL: "https://example.com/s/NEW"},
		&CloudTask{Status: "failed"})
	if err != nil || opts.keepDownloadDone || opts.force {
		t.Fatalf("换链接重试应与 retry_download 同语义（保留断点），got %+v, %v", opts, err)
	}
}

// TestAdoptURL 锁定「换链接」的源列表语义：新 URL 置顶，旧 URL 保留为备选。
func TestAdoptURL(t *testing.T) {
	t.Parallel()
	task := &CloudTask{URL: "https://a/s/1", FallbackSources: []string{"https://a/s/1", "https://b/s/2"}, SourceIndex: 1,
		Diagnostics: &TaskDiagnostics{Attempts: 3}}
	task.adoptURL("https://c/s/3")
	if task.URL != "https://c/s/3" || task.SourceIndex != 0 {
		t.Fatalf("新 URL 应置顶且 index=0，got url=%s idx=%d", task.URL, task.SourceIndex)
	}
	want := []string{"https://c/s/3", "https://a/s/1", "https://b/s/2"}
	if len(task.FallbackSources) != len(want) {
		t.Fatalf("备选源 = %v, want %v", task.FallbackSources, want)
	}
	for i := range want {
		if task.FallbackSources[i] != want[i] {
			t.Fatalf("备选源 = %v, want %v", task.FallbackSources, want)
		}
	}
	if task.Diagnostics.Attempts != 0 {
		t.Fatal("新源应重置尝试次数")
	}
	// 幂等：同 URL 不改动
	before := append([]string(nil), task.FallbackSources...)
	task.adoptURL("https://c/s/3")
	if len(task.FallbackSources) != len(before) {
		t.Fatal("同 URL 不应改动源列表")
	}
}

// TestApplyTaskAction_RetryTransferOverwriteSetsFlag 锁定「覆盖转存」真的把 Overwrite 打开。
func TestApplyTaskAction_RetryTransferOverwriteSetsFlag(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	lfs := syncpkg.NewLocalFS(root, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-OW-1", "ow.bin", 1024, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v", Path: "ow/out.bin"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
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
	mgr.mu.Lock()
	task.Status = "failed"
	task.DownloadDone = true
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	if _, err := mgr.ApplyTaskAction(task.ID, TaskActionRetryTransferOverwrite, ""); err != nil {
		t.Fatalf("ApplyTaskAction: %v", err)
	}
	mgr.mu.RLock()
	got := mgr.tasks[task.ID]
	ow := got.Transfer != nil && got.Transfer.Overwrite
	mgr.mu.RUnlock()
	if !ow {
		t.Fatal("覆盖重试必须置 Transfer.Overwrite=true")
	}
}

// TestApplyTaskAction_RetryFromURL 锁定「换链接重试」采用新 URL 且保留进度。
func TestApplyTaskAction_RetryFromURL(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())
	const oldURL, newURL = "https://example.com/s/EXAMPLE-URL-1", "https://example.com/s/EXAMPLE-URL-2"
	task, err := mgr.CreateTask("url", oldURL, "u.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = "failed"
	task.Downloaded = 700
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()

	if _, err := mgr.ApplyTaskActionRequest(task.ID, TaskActionRequest{Action: TaskActionRetryFromURL, URL: newURL}, ""); err != nil {
		t.Fatalf("ApplyTaskActionRequest: %v", err)
	}
	mgr.mu.RLock()
	got := mgr.tasks[task.ID]
	url, idx, fb := got.URL, got.SourceIndex, append([]string(nil), got.FallbackSources...)
	mgr.mu.RUnlock()
	if url != newURL || idx != 0 {
		t.Fatalf("应采用新 URL 且 index=0，got url=%s idx=%d", url, idx)
	}
	if len(fb) != 2 || fb[1] != oldURL {
		t.Fatalf("旧 URL 应保留为备选源，got %v", fb)
	}
}

// TestRetryDownloadHint_Honest 锁定「续传下载」提示必须反映磁盘真实情况
// （2026-11-26 实测：取消后残留已删，提示不能再说"从断点继续"）。
func TestRetryDownloadHint_Honest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		task *CloudTask
		want string
	}{
		{"有保留断点", &CloudTask{Status: "failed", Diagnostics: &TaskDiagnostics{PartialSize: 15 << 20}},
			"保留已下载部分，从断点继续（不重下已完成块）"},
		{"产物已完整（转存失败）", &CloudTask{Status: "failed", DownloadDone: true, Diagnostics: &TaskDiagnostics{PartialSize: 64 << 20}},
			"重新下载整个文件（产物已完整时请改用「仅重做转存」，避免重复下载）"},
		{"取消已清理残留", &CloudTask{Status: "cancelled", Downloaded: 9 << 20},
			"从零重新下载（无可续断点：上次结束时残留已清理）"},
		{"从未下载", &CloudTask{Status: "failed"}, "从零重新下载（无可续断点：上次结束时残留已清理）"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := retryDownloadHint(c.task); got != c.want {
				t.Fatalf("hint = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPlanTaskActions_HintWired 锁定计划里用的是诚实提示（不是固定文案）。
func TestPlanTaskActions_HintWired(t *testing.T) {
	t.Parallel()
	plan := PlanTaskActions(&CloudTask{Status: "failed"})
	if len(plan) == 0 || plan[0].Action != TaskActionRetryDownload {
		t.Fatalf("首个动作应为 retry_download: %+v", plan)
	}
	if !strings.Contains(plan[0].Hint, "无可续断点") {
		t.Fatalf("无断点时应明确提示从零重下，got %q", plan[0].Hint)
	}
}
