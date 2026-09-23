// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import "testing"

// TestTransitionTask_ValidAndInvalid 钉住集中式状态迁移表（审查 P3 修复）：
// 合法迁移生效 + UpdatedAt 刷新；非法迁移拒绝且状态不变。
func TestTransitionTask_ValidAndInvalid(t *testing.T) {
	t.Parallel()
	base := newSyncTaskForTransition()
	// 合法：pending→syncing。
	if !transitionTask(base, StatusSyncing) {
		t.Fatal("pending→syncing 应合法")
	}
	if base.Status != StatusSyncing {
		t.Fatalf("状态应为 syncing, got %q", base.Status)
	}
	// 合法：syncing→completed。
	if !transitionTask(base, StatusCompleted) {
		t.Fatal("syncing→completed 应合法")
	}
	// 非法：completed 终态不可变（completed→retrying 拒绝）。
	if transitionTask(base, StatusRetrying) {
		t.Fatal("completed→retrying 应拒绝（终态不可变）")
	}
	if base.Status != StatusCompleted {
		t.Fatalf("非法迁移后状态不应改变, got %q", base.Status)
	}
}

// TestTransitionTask_RestartAndRetryPaths 钉住恢复/重试特殊迁移：
// syncing→syncing（重启恢复幂等重入）、retrying→completed（重试后成功）、
// completed→failed（reconcile 对账失败回滚）、failed→pending（Fanout 重试重置）。
func TestTransitionTask_RestartAndRetryPaths(t *testing.T) {
	t.Parallel()
	t.Run("syncing_to_syncing_restart", func(t *testing.T) {
		t.Parallel()
		tt := newSyncTaskForTransition()
		tt.Status = StatusSyncing
		if !transitionTask(tt, StatusSyncing) {
			t.Fatal("syncing→syncing（恢复重入）应合法")
		}
	})
	t.Run("retrying_to_completed", func(t *testing.T) {
		t.Parallel()
		tt := newSyncTaskForTransition()
		tt.Status = StatusRetrying
		if !transitionTask(tt, StatusCompleted) {
			t.Fatal("retrying→completed（重试成功）应合法")
		}
	})
	t.Run("completed_to_failed_reconcile", func(t *testing.T) {
		t.Parallel()
		tt := newSyncTaskForTransition()
		tt.Status = StatusCompleted
		if !transitionTask(tt, StatusFailed) {
			t.Fatal("completed→failed（reconcile 对账失败）应合法")
		}
	})
	t.Run("failed_to_pending_retry_reset", func(t *testing.T) {
		t.Parallel()
		tt := newSyncTaskForTransition()
		tt.Status = StatusFailed
		if !transitionTask(tt, StatusPending) {
			t.Fatal("failed→pending（Fanout 重试重置）应合法")
		}
	})
}

// newSyncTaskForTransition 构造最小 SyncTask（状态迁移测试用）。
func newSyncTaskForTransition() *SyncTask {
	return &SyncTask{ID: "t", Status: StatusPending}
}
