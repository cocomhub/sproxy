// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
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
