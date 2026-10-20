// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// prod_hardening_test.go 钉住 2026-10-21 对抗审查 P1：孤儿组重建必须**按 GroupID 聚合**。
//
// 此前 `recoverOrphanGroups` 在 `m.groups` 尚未填充的扫描阶段逐任务建组，且
// `TaskIDs` 只含当前任务、`TotalTasks=1` ⇒ 同组 N 个任务只留下最后一个 ⇒ 组视图丢成员、
// 单个成员完成即判整组 completed、组级 cancel/archive 只作用于 1 个任务（其余继续占用
// 并发槽与配额，直到各自超时）。可达路径：CreateGroup 先建子任务、最后才 saveGroup 的窗口
// 崩溃/磁盘满，或组文件损坏被跳过。

import (
	"context"
	"slices"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestRecoverOrphanGroups_AggregatesSameGroup 同组 N 个孤儿任务必须重建为**一个**含 N 个成员的组。
func TestRecoverOrphanGroups_AggregatesSameGroup(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	const gid = "g-orphan-1"
	const n = 3
	ids := make([]string, 0, n)
	for range n {
		id := seedPersistedTask(t, mgr, "pending", true)
		mgr.mu.Lock()
		if task := mgr.tasks[id]; task != nil {
			task.GroupID = gid
		}
		task := mgr.tasks[id]
		mgr.mu.Unlock()
		if task == nil {
			t.Fatalf("任务 %s 不在 map 中", id)
		}
		if serr := mgr.saveTask(task); serr != nil {
			t.Fatalf("saveTask: %v", serr)
		}
		ids = append(ids, id)
	}
	// 模拟「组记录缺失」：组表清空后重建。
	mgr.groupMu.Lock()
	delete(mgr.groups, gid)
	mgr.groupMu.Unlock()

	if got := mgr.recoverOrphanGroups(); got != 1 {
		t.Fatalf("恢复组数 = %d, want 1（同组多任务必须聚合成一个）", got)
	}

	mgr.groupMu.RLock()
	group := mgr.groups[gid]
	mgr.groupMu.RUnlock()
	if group == nil {
		t.Fatalf("组 %s 未重建", gid)
	}
	if group.TotalTasks != n || len(group.TaskIDs) != n {
		t.Fatalf("组必须含全部 %d 个成员, got total=%d taskIDs=%v", n, group.TotalTasks, group.TaskIDs)
	}
	for _, id := range ids {
		found := slices.Contains(group.TaskIDs, id)
		if !found {
			t.Fatalf("组成员缺失 %s: %v", id, group.TaskIDs)
		}
	}
}

// TestRecoverAll_CanceledContextStopsRecovery 启动恢复必须**可被取消**（装配层的
// startupRecoveryTimeout 只有在 ctx 真正下传到恢复循环时才有效；恢复是
// ListenAndServe 之前同步执行 ⇒ 挂住会让服务永不监听）。
func TestRecoverAll_CanceledContextStopsRecovery(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	// 落两条任务（已提交的 downloading ⇒ 正常会被重启）。
	for i := 0; i < 2; i++ {
		seedPersistedTask(t, mgr, "downloading", true)
	}
	mgr.mu.Lock()
	for id := range mgr.tasks {
		delete(mgr.tasks, id)
	}
	mgr.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入恢复前即取消 ⇒ 必须立刻停止，不重启任何任务
	n, err := mgr.RecoverAll(ctx)
	if err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
	if n != 0 {
		t.Fatalf("已取消的 ctx 不应恢复任何任务, got %d", n)
	}
	mgr.mu.RLock()
	left := len(mgr.tasks)
	mgr.mu.RUnlock()
	if left != 0 {
		t.Fatalf("已取消的 ctx 不应插入任何任务, got %d", left)
	}
}
