// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"testing"
	"time"
)

// TestFanout_CreateMultipleRemotes 验证扇出：多 remote → 每 remote 一个子任务
// （mock 执行器断言每 remote 一次 Run）。
func TestFanout_CreateMultipleRemotes(t *testing.T) {
	t.Parallel()
	remotes := []RemoteConfig{
		testRemote("r1", "http://127.0.0.1:1"),
		testRemote("r2", "http://127.0.0.1:2"),
		testRemote("r3", "http://127.0.0.1:3"),
	}
	exec := newMockExecutor(completedResult())
	mgr := newTestManager(t, nil, remotes, exec, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})

	// 红灯条件：CreateRequest.Remotes 字段尚未支持（编译/行为失败）。
	parent, _, err := mgr.CreateTask(CreateRequest{
		Direction: string(DirectionPush),
		Remotes:   []string{"r1", "r2", "r3"},
		Src:       "x.txt", Dst: "",
	})
	if err != nil {
		t.Fatalf("CreateTask 扇出失败: %v", err)
	}
	if parent.Remote != "" {
		t.Fatalf("父任务 Remote 应为空（聚合视图）, got %q", parent.Remote)
	}
	// 每 remote 子任务存在。
	for _, r := range []string{"r1", "r2", "r3"} {
		child := mgr.GetFanoutChild(parent.ID, r)
		if child == nil {
			t.Fatalf("缺少 remote %s 的子任务", r)
		}
		if child.Remote != r {
			t.Fatalf("子任务 remote = %q, want %q", child.Remote, r)
		}
	}
}

// TestFanout_SingleRemoteZeroRegression 验证单 Remote 兼容零回归（不扇出）。
func TestFanout_SingleRemoteZeroRegression(t *testing.T) {
	t.Parallel()
	exec := newMockExecutor(completedResult())
	mgr := newTestManager(t, nil, nil, exec, &Config{MaxConcurrent: 3, TaskTTL: time.Hour})

	task, _, err := mgr.CreateTask(CreateRequest{
		Direction: string(DirectionPush),
		Remote:    "r1",
		Src:       "x.txt", Dst: "",
	})
	if err != nil {
		t.Fatalf("CreateTask 单值: %v", err)
	}
	if task.Remote != "r1" {
		t.Fatalf("单值任务 remote = %q, want r1（零回归）", task.Remote)
	}
	if mgr.GetFanoutChild(task.ID, "r1") != nil {
		t.Fatalf("单值任务不应有扇出子任务")
	}
}

// TestFanout_SingleTargetFailureIndependent 验证单目标失败不阻塞其它：
// remote A 执行失败 → remote B/C 仍完成。
func TestFanout_SingleTargetFailureIndependent(t *testing.T) {
	t.Parallel()
	remotes := []RemoteConfig{
		testRemote("r1", "http://127.0.0.1:1"),
		testRemote("r2", "http://127.0.0.1:2"),
	}
	exec := newMockExecutor(completedResult())
	mgr := newTestManager(t, nil, remotes, exec, &Config{MaxConcurrent: 2, TaskTTL: time.Hour})

	parent, _, err := mgr.CreateTask(CreateRequest{
		Direction: string(DirectionPush),
		Remotes:   []string{"r1", "r2"},
		Src:       "x.txt", Dst: "",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// r1 子任务注入失败。
	if ferr := mgr.FailFanoutChildForTest(parent.ID, "r1", "网络中断"); ferr != nil {
		t.Fatalf("注入 r1 失败: %v", ferr)
	}
	if got := mgr.FanoutChildStatus(parent.ID, "r1"); got != StatusFailed {
		t.Fatalf("r1 子任务应 failed, got %q", got)
	}
	if got := mgr.FanoutChildStatus(parent.ID, "r2"); got == StatusFailed {
		t.Fatalf("r2 子任务不应受 r1 失败影响, got %q", got)
	}
	// 父任务不因子任务失败变 failed（聚合视图保持 pending，单目标失败独立）。
	if got := mgr.Get(parent.ID, "").Status; got == StatusFailed {
		t.Fatalf("父任务不应因单子任务失败变 failed, got %q", got)
	}
	// 汇总视图正确反映单目标失败（Failed=1，Completed/Pending 含另一子任务）。
	sum := mgr.FanoutSummaryOf(parent.ID)
	if sum.Failed != 1 {
		t.Fatalf("汇总 Failed 应为 1（r1 失败）, got %d (children=%+v)", sum.Failed, sum.Children)
	}
}

// TestFanout_RetryRemote 验证失败节点独立重试（RetryRemote 只重试失败 remote 的子任务）。
func TestFanout_RetryRemote(t *testing.T) {
	t.Parallel()
	remotes := []RemoteConfig{
		testRemote("r1", "http://127.0.0.1:1"),
		testRemote("r2", "http://127.0.0.1:2"),
	}
	exec := newMockExecutor(completedResult())
	mgr := newTestManager(t, nil, remotes, exec, &Config{MaxConcurrent: 2, TaskTTL: time.Hour})

	parent, _, err := mgr.CreateTask(CreateRequest{
		Direction: string(DirectionPush),
		Remotes:   []string{"r1", "r2"},
		Src:       "x.txt", Dst: "",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if ferr := mgr.FailFanoutChildForTest(parent.ID, "r1", "网络中断"); ferr != nil {
		t.Fatalf("注入失败: %v", ferr)
	}
	// RetryRemote 只重试 r1（失败节点）。
	res, err := mgr.RetryRemote(context.Background(), parent.ID, "", "r1")
	if err != nil {
		t.Fatalf("RetryRemote: %v", err)
	}
	if !res.Retried {
		t.Fatalf("r1 应被重试, got %+v", res)
	}
	// 重试后 r1 子任务状态回 pending（重新排队）。
	if got := mgr.FanoutChildStatus(parent.ID, "r1"); got != StatusPending && got != StatusSyncing && got != StatusCompleted {
		t.Fatalf("重试后 r1 子任务状态异常: %q", got)
	}

	// 成功节点（r2 未失败）不应被重试（幂等跳过）。
	res2, err := mgr.RetryRemote(context.Background(), parent.ID, "", "r2")
	if err != nil {
		t.Fatalf("RetryRemote(r2): %v", err)
	}
	if res2.Retried {
		t.Fatalf("成功节点 r2 不应被重试, got %+v", res2)
	}
}
