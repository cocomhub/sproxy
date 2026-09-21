// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRetryFiles_RetriesOnlyFailed 验证 RetryFiles 只重试失败文件：
// 任务 Results 含 created（成功）与 error（失败）→ 指定失败文件重试 → 重试子任务
// Include 只含该文件；成功文件未被重试。
func TestRetryFiles_RetriesOnlyFailed(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, nil, nil, nil, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	exec := mgr.executor.(*mockExecutor)

	// 预置一个含失败结果的任务（结果含 error 与 created）。
	task, _, err := mgr.SubmitAndStart(CreateRequest{
		Direction: string(DirectionPush), Remote: "r1", Src: "x.txt", Dst: "",
		Owner: "alice",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	waitForStatus(t, mgr, task.ID, StatusCompleted, 30*time.Second)
	// 回填失败结果（模拟真实执行后含失败文件）。
	mgr.mu.Lock()
	stored := mgr.tasks[task.ID]
	stored.Results = []SyncFileResult{
		{Path: "ok.txt", Action: "created", Size: 5},
		{Path: "bad.txt", Action: "error", Error: "写入失败"},
		{Path: "verify.txt", Action: "verify_failed", Error: "checksum 不一致"},
	}
	stored.Status = StatusFailed // 重试针对失败任务
	stored.FilesTotal = 3
	mgr.mu.Unlock()

	// 指定单个失败文件重试（bad.txt）。
	res, err := mgr.RetryFiles(context.Background(), task.ID, "alice", []string{"bad.txt"})
	if err != nil {
		t.Fatalf("RetryFiles: %v", err)
	}
	if len(res.Retried) != 1 || res.Retried[0].Path != "bad.txt" {
		t.Fatalf("应重试 1 个文件 bad.txt, got %+v", res)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("不应有跳过文件, got %+v", res.Skipped)
	}

	// 重试子任务 Include 只含 bad.txt（glob 精确匹配该文件）。
	exec.mu.Lock()
	last := exec.lastTask
	exec.mu.Unlock()
	if last == nil || len(last.Include) != 1 || last.Include[0] != "bad.txt" {
		t.Fatalf("重试子任务 Include 应只含 bad.txt, got %+v", last)
	}
	// 子任务方向/remote/src/dst 与源任务一致。
	if last.Direction != string(DirectionPush) || last.Remote != "r1" || last.Src != "x.txt" {
		t.Fatalf("重试子任务参数应与源任务一致, got %+v", last)
	}
}

// TestRetryFiles_EmptyFilesRetriesAllFailed 验证 files 为空 = 重试全部失败文件。
func TestRetryFiles_EmptyFilesRetriesAllFailed(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, nil, nil, nil, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	exec := mgr.executor.(*mockExecutor)

	task, _, err := mgr.SubmitAndStart(CreateRequest{
		Direction: string(DirectionPush), Remote: "r1", Src: "x.txt", Dst: "",
		Owner: "alice",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	waitForStatus(t, mgr, task.ID, StatusCompleted, 30*time.Second)
	mgr.mu.Lock()
	stored := mgr.tasks[task.ID]
	stored.Results = []SyncFileResult{
		{Path: "a.txt", Action: "error", Error: "网络中断"},
		{Path: "b.txt", Action: "verify_failed", Error: "checksum 不一致"},
		{Path: "ok.txt", Action: "created", Size: 5},
	}
	stored.Status = StatusFailed
	mgr.mu.Unlock()

	res, err := mgr.RetryFiles(context.Background(), task.ID, "alice", nil)
	if err != nil {
		t.Fatalf("RetryFiles: %v", err)
	}
	if len(res.Retried) != 2 {
		t.Fatalf("空 files 应重试全部失败文件（2 个）, got %+v", res)
	}
	got := map[string]bool{}
	for _, r := range res.Retried {
		got[r.Path] = true
	}
	if !got["a.txt"] || !got["b.txt"] {
		t.Fatalf("应包含 a.txt/b.txt, got %+v", got)
	}
	exec.mu.Lock()
	last := exec.lastTask
	exec.mu.Unlock()
	if last == nil || len(last.Include) != 2 {
		t.Fatalf("重试子任务 Include 应含 2 个失败文件, got %+v", last)
	}
}

// TestRetryFiles_InvalidFileSkipped 验证幂等：指定非失败/不存在文件 → skipped。
func TestRetryFiles_InvalidFileSkipped(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, nil, nil, nil, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})

	task, _, err := mgr.SubmitAndStart(CreateRequest{
		Direction: string(DirectionPush), Remote: "r1", Src: "x.txt", Dst: "",
		Owner: "alice",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	waitForStatus(t, mgr, task.ID, StatusCompleted, 30*time.Second)
	mgr.mu.Lock()
	stored := mgr.tasks[task.ID]
	stored.Results = []SyncFileResult{
		{Path: "bad.txt", Action: "error", Error: "写入失败"},
		{Path: "ok.txt", Action: "created", Size: 5},
	}
	stored.Status = StatusFailed
	mgr.mu.Unlock()

	// 指定成功文件（ok.txt）+ 不存在文件（ghost.txt）→ 全 skipped。
	res, err := mgr.RetryFiles(context.Background(), task.ID, "alice", []string{"ok.txt", "ghost.txt"})
	if err != nil {
		t.Fatalf("RetryFiles: %v", err)
	}
	if len(res.Retried) != 0 {
		t.Fatalf("成功/不存在文件不应重试, got %+v", res.Retried)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("应跳过 2 个文件, got %+v", res.Skipped)
	}
}

// TestRetryFiles_CrossOwnerNotFound 验证跨 owner 重试返回 ErrNotFound（IDOR 防护）。
func TestRetryFiles_CrossOwnerNotFound(t *testing.T) {
	t.Parallel()
	mgr := newTestManager(t, nil, nil, nil, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})

	task, _, err := mgr.SubmitAndStart(CreateRequest{
		Direction: string(DirectionPush), Remote: "r1", Src: "x.txt", Dst: "",
		Owner: "alice",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	waitForStatus(t, mgr, task.ID, StatusCompleted, 30*time.Second)

	// bob 看不到 alice 的任务。
	if _, err := mgr.RetryFiles(context.Background(), task.ID, "bob", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("跨 owner 重试应返回 not found, got %v", err)
	}
}
