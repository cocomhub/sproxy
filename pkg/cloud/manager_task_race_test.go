// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_task_race_test.go 锁「失败产物处置」与「后台持久化」之间的**并发安全**。
//
// 历史缺陷（2026-11-26 CI 命中，Test(windows) / Test(ubuntu,+Vault) 的 -race 报警）：
//   - `flushDirty → saveTask` 在 `m.mu.RLock()` 下 `json.Marshal(task)`（读 Diagnostics 等字段）；
//   - 但 `settleArtifactsOnFail` 在**释放锁之后**写 `task.Diagnostics.PartialPath/PartialSize`；
//   - RLock 保护不了「根本不持锁的写方」⇒ 数据竞争。
//
// 本仓既有约定（`task_diagnostics.go` 的 `noteStage`/`noteFailure` 注释）是「调用方须持有 m.mu」；
// `settleArtifactsOnFail` 违反了它。本用例把这条约定钉成可执行断言。

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestSettleArtifactsOnFail_NoRaceWithSaveTask 并发驱动「失败产物处置」与「持久化」，
// 在 -race 下必须无告警。
//
// 说明：竞争只在**真并发**下才被检测到，因此这里用多轮 + 两个 goroutine 反复触发；
// 修复前 `-race` 会报 `Read at ... saveTask / Previous write at ... settleArtifactsOnFail`。
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
	if merr := mkdirAllAndWritePartial(dir, task.Filename); merr != nil {
		t.Fatalf("造断点: %v", merr)
	}
	mgr.mu.Lock()
	task.Downloaded = 2048
	mgr.mu.Unlock()

	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(2)
	// 写方：失败产物处置（会写 Diagnostics；修复后必须不在此处无锁写 task）。
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			mgr.settleArtifactsOnFail(task, "source unreachable: i/o timeout", nil)
		}
	}()
	// 读方：后台持久化（json.Marshal 读整条 task）。
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = mgr.saveTask(task)
		}
	}()
	wg.Wait()
}
