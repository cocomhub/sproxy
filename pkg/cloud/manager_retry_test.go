// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClassifyCloudError 锁定失败归类（决定重试策略）：各类错误必须归到稳定类别。
func TestClassifyCloudError(t *testing.T) {
	t.Parallel()
	cases := []struct{ msg, want string }{
		{"context deadline exceeded", ErrClassSourceUnreachable},
		{"dial tcp: connection refused", ErrClassSourceUnreachable},
		{"HTTP 503 service unavailable", ErrClassSourceUnreachable},
		{"百度返回错误 代码: 31034", ErrClassThrottled},
		{"too many requests", ErrClassThrottled},
		{"checksum mismatch: want abc got def", ErrClassContentMismatch},
		{"integrity verify failed", ErrClassContentMismatch},
		{"target file already exists", ErrClassTargetConflict},
		{"no space left on device", ErrClassDiskFull},
		{"转存失败: 目标卷 \"baidu\" 未装配", ErrClassTargetUnavailable},
		{"volume not found: v", ErrClassTargetUnavailable},
		{"HTTP 401 unauthorized", ErrClassAuth},
		{"something weird", ErrClassUnknown},
		{"", ErrClassUnknown},
	}
	for _, c := range cases {
		if got := classifyCloudError(c.msg); got != c.want {
			t.Errorf("classifyCloudError(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

// TestNoteFailure_FillsDiagnostics 锁定失败诊断快照：归类/时间/原文/部分产物都要记录。
func TestNoteFailure_FillsDiagnostics(t *testing.T) {
	t.Parallel()
	task := &CloudTask{ID: "t1", Status: "downloading"}
	task.noteStage("downloading")
	task.noteFailure("checksum mismatch for file", 1024, "t1")
	d := task.Diag()
	if d.Stage != "downloading" {
		t.Fatalf("stage = %q", d.Stage)
	}
	if d.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", d.Attempts)
	}
	if d.LastErrorClass != ErrClassContentMismatch {
		t.Fatalf("class = %q", d.LastErrorClass)
	}
	if d.PartialSize != 1024 || d.PartialPath != "t1" {
		t.Fatalf("partial = %d/%q", d.PartialSize, d.PartialPath)
	}
	if d.LastErrorAt.IsZero() {
		t.Fatal("LastErrorAt 必须记录")
	}
}

// TestSelectRetryable_FiltersByTransferPath 锁定「按目标路径重试」的选择语义（修复场景主用法）。
func TestSelectRetryable_FiltersByTransferPath(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{tasks: map[string]*CloudTask{
		"a": {ID: "a", Status: "failed", URL: "u1", Transfer: &TransferSpec{Volume: "v", Path: "x/1.mp4"}},
		"b": {ID: "b", Status: "failed", URL: "u2", Transfer: &TransferSpec{Volume: "v", Path: "x/2.mp4"}},
		"c": {ID: "c", Status: "completed", URL: "u3", Transfer: &TransferSpec{Volume: "v", Path: "x/3.mp4"}},
	}}
	got := m.SelectRetryable(RetrySelector{TransferPath: "x/1.mp4"})
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("按路径选择错误: %v", got)
	}
	// completed（且非 damaged）不可重试。
	if got := m.SelectRetryable(RetrySelector{TransferPath: "x/3.mp4"}); len(got) != 0 {
		t.Fatalf("completed 不应被重试: %v", got)
	}
	// 所有 failed 任务。
	if got := m.SelectRetryable(RetrySelector{}); len(got) != 2 {
		t.Fatalf("应选中 2 个 failed 任务，got %v", got)
	}
}

// TestDefaultFailedTaskTTL 锁定「失败任务保留 7d」（2026-11-21 用户裁定）：
// 此前默认 1h ⇒ 失败任务很快消失，用户既看不到也重试不了。变异：改回 1h ⇒ 本用例红。
func TestDefaultFailedTaskTTL(t *testing.T) {
	t.Parallel()
	cfg := CloudDownloadConfig{}
	applyCloudConfigDefaults(&cfg)
	if cfg.FailedTaskTTL != 7*24*time.Hour {
		t.Fatalf("FailedTaskTTL 默认 = %v，期望 7d", cfg.FailedTaskTTL)
	}
}

// TestObserveFailure_NotifiesObserver 锁定失败观察者（装配层据此登记事项；2026-11-21）。
// 观察者必须收到：任务 ID、转存目标、失败归类与累计次数。
func TestObserveFailure_NotifiesObserver(t *testing.T) {
	t.Parallel()
	var got TaskFailure
	m := &CloudDownloadManager{}
	m.SetTaskFailureObserver(func(f TaskFailure) { got = f })
	task := &CloudTask{ID: "t9", Owner: "o", URL: "u",
		Transfer: &TransferSpec{Volume: "v", Path: "a/b.mp4"}}
	task.noteStage("downloading")
	task.noteFailure("context deadline exceeded", 0, "")
	m.observeFailure(task)
	if got.TaskID != "t9" || got.Volume != "v" || got.Path != "a/b.mp4" {
		t.Fatalf("观察者载荷错误: %+v", got)
	}
	if got.ErrorClass != ErrClassSourceUnreachable || got.Attempts != 1 {
		t.Fatalf("类别/次数错误: %+v", got)
	}
}

// TestRotateSource 锁定「换源」策略（2026-11-21 设计 §3.4）：
// 源类失败/内容不一致且有备选源 ⇒ 切到下一条并把尝试计数归零；其它类别或没有备选 ⇒ 不切。
func TestRotateSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		class  string
		fb     []string
		idx    int
		want   bool
		wantIn int
	}{
		{"源不可达且有备选", ErrClassSourceUnreachable, []string{"u0", "u1"}, 0, true, 1},
		{"限流且有备选", ErrClassThrottled, []string{"u0", "u1"}, 0, true, 1},
		{"内容不一致且有备选", ErrClassContentMismatch, []string{"u0", "u1"}, 0, true, 1},
		{"已用尽备选", ErrClassSourceUnreachable, []string{"u0"}, 0, false, 0},
		{"磁盘满不换源", ErrClassDiskFull, []string{"u0", "u1"}, 0, false, 0},
		{"无备选源", ErrClassSourceUnreachable, nil, 0, false, 0},
	}
	for _, c := range cases {
		task := &CloudTask{URL: "u0", FallbackSources: c.fb, SourceIndex: c.idx}
		task.noteFailure("x", 0, "")
		task.Diagnostics.LastErrorClass = c.class
		if got := task.rotateSourceLocked(); got != c.want {
			t.Errorf("%s: rotate=%v want %v", c.name, got, c.want)
		}
		if task.SourceIndex != c.wantIn {
			t.Errorf("%s: SourceIndex=%d want %d", c.name, task.SourceIndex, c.wantIn)
		}
		if c.want && task.URL != c.fb[c.wantIn] {
			t.Errorf("%s: URL 未切换，got %s", c.name, task.URL)
		}
	}
}

// TestVerifyExpectedChecksum 锁定「转存前期望校验」：不一致必须报错（→ content_mismatch 重试）。
func TestVerifyExpectedChecksum(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{tasks: map[string]*CloudTask{}}
	task := &CloudTask{ID: "t", ExpectedChecksum: "abc"}
	if err := m.verifyExpectedChecksum(task, &downloaderResultView{Checksum: "abc"}, ""); err != nil {
		t.Fatalf("一致时应通过: %v", err)
	}
	if err := m.verifyExpectedChecksum(task, &downloaderResultView{Checksum: "def"}, ""); err == nil {
		t.Fatal("不一致必须报错")
	}
	// 未指定期望值 ⇒ 不额外校验（零回归）。
	if err := m.verifyExpectedChecksum(&CloudTask{ID: "t2"}, &downloaderResultView{Checksum: "x"}, ""); err != nil {
		t.Fatalf("未指定期望值不应报错: %v", err)
	}
}

// TestVerifyExpectedChecksum_FailClosed 锁定 fail-closed（2026-11-24 实测缺口）：
// 调用方给了期望值，但下载器未提供 checksum 且本地也无法计算 ⇒ **必须判失败**，
// 绝不能像早期实现那样"无 checksum 即跳过"（实测导致错误期望值的探针任务被判 completed）。
func TestVerifyExpectedChecksum_FailClosed(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{tasks: map[string]*CloudTask{}}
	task := &CloudTask{ID: "t", ExpectedChecksum: "abc"}
	if err := m.verifyExpectedChecksum(task, &downloaderResultView{}, ""); err == nil {
		t.Fatal("有期望值但 checksum 不可得时必须失败（fail-closed）")
	}
	// 本地补算路径：真实文件内容 ≠ 期望值 ⇒ 失败。
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := m.verifyExpectedChecksum(task, &downloaderResultView{}, p); err == nil {
		t.Fatal("本地补算后不一致必须失败")
	}
	// 补算一致 ⇒ 通过。
	sum := sha256.Sum256([]byte("hello"))
	task2 := &CloudTask{ID: "t2", ExpectedChecksum: hex.EncodeToString(sum[:])}
	if err := m.verifyExpectedChecksum(task2, &downloaderResultView{}, p); err != nil {
		t.Fatalf("本地补算一致应通过: %v", err)
	}
}

// TestClassifyCloudError_TargetConflictCN 锁定中文幂等冲突归类（2026-11-24 实测：
// 转存失败文案「已存在且幂等」此前归 unknown ⇒ 不可识别为 target_conflict）。
func TestClassifyCloudError_TargetConflictCN(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		`transfers: 转存目标 "a/b.mp4" 已存在且幂等`,
		"target already exists",
	} {
		if got := classifyCloudError(msg); got != ErrClassTargetConflict {
			t.Errorf("classifyCloudError(%q) = %q, want %q", msg, got, ErrClassTargetConflict)
		}
	}
}

// TestWantOverwrite 锁定覆盖语义的取值（默认 false = 目标唯一；显式 true 才覆盖）。
func TestWantOverwrite(t *testing.T) {
	t.Parallel()
	if wantOverwrite(nil) || wantOverwrite(&CloudTask{}) {
		t.Fatal("无 TransferSpec 时不得覆盖")
	}
	if wantOverwrite(&CloudTask{Transfer: &TransferSpec{Volume: "v"}}) {
		t.Fatal("未显式 Overwrite 时不得覆盖（默认目标唯一）")
	}
	if !wantOverwrite(&CloudTask{Transfer: &TransferSpec{Volume: "v", Overwrite: true}}) {
		t.Fatal("显式 Overwrite=true 时应允许覆盖")
	}
}
