// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_task_race_test.go 是**并发安全回归测试**，锁住「失败产物处置」与「后台持久化」
// 之间不得有数据竞争。
//
// 背景（2026-11-26 CI `-race` 命中，已由 `recordKeptPartial` 持锁修好）：
//   - `flushDirty → saveTask` 在 `m.mu.RLock()` 下 `json.Marshal(task)`（读 Diagnostics 等字段）；
//   - 而 `settleArtifactsOnFail` 曾在 `failTask` **释放锁之后**写
//     `task.Diagnostics.PartialPath/PartialSize`；
//   - RLock 保护不了「根本不持锁的写方」⇒ 数据竞争。
//
// 仓内既有约定（`task_diagnostics.go` 的 `noteStage`/`noteFailure` 注释）是「调用方须持有
// m.mu」。本用例把这条约定钉成**可执行断言**：谁再在锁外写 task 字段，`-race` 立刻报。
//
// 为什么必须保留这条测试：修复本身（`recordKeptPartial` 取锁）没有其它用例覆盖——
// 正常功能用例单线程执行，永远看不到竞争。只有真并发 + `-race` 才能守住。

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestSettleArtifactsOnFail_NoRaceWithSaveTask 并发驱动「失败产物处置」与「持久化」，
// 在 -race 下必须无告警。
func TestSettleArtifactsOnFail_NoRaceWithSaveTask(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig())

	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-RACE-1", "race.bin", 4096, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	dir := mgr.TaskDirFor(task.Owner, task.ID)
	if dir == "" {
		t.Skip("租户目录不可用（测试环境）")
	}
	// 造 .partial：让 settle 走「保留断点 + 写诊断」这条会碰 task 字段的路径。
	if merr := mkdirAllAndWritePartial(dir, task.Filename); merr != nil {
		t.Fatalf("造断点: %v", merr)
	}
	mgr.mu.Lock()
	task.Downloaded = 2048
	mgr.mu.Unlock()

	// 竞争只在真并发下才被检测到 ⇒ 多轮 + 两个 goroutine 反复触发。
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	// 写方：失败产物处置（**修复前**在此无锁写 Diagnostics）。
	//
	// 每轮把 Downloaded 推进 1 字节：诊断写入是 `if Downloaded > d.PartialSize` 守卫的，
	// 不推进就只在**首轮**写一次 ⇒ 竞争窗口太窄，`-race` 可能观察不到（实测漏检）。
	go func() {
		defer wg.Done()
		for range rounds {
			mgr.mu.Lock()
			task.Downloaded++
			mgr.mu.Unlock()
			mgr.settleArtifactsOnFail(task, "source unreachable: i/o timeout", nil)
		}
	}()
	// 读方：后台持久化（json.Marshal 读整条 task，含 Diagnostics）。
	go func() {
		defer wg.Done()
		for range rounds {
			_ = mgr.saveTask(task)
		}
	}()
	wg.Wait()

	// 顺带断言语义没被"为消除竞争而跳过写入"破坏：诊断仍要留下断点信息。
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if task.Diagnostics == nil || task.Diagnostics.PartialPath == "" {
		t.Fatalf("保留断点后必须记录 PartialPath: %+v", task.Diagnostics)
	}
}
