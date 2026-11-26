// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// list_tasks_snapshot_race_test.go 锁「ListTasks 的快照必须深拷贝」。
//
// 背景（2026-11-27 对抗审查发现）：#1327/#1329 把 `SnapshotTask` / `SnapshotTasks` 改成
// `deepCopyTask`，但 **`ListTasks` 仍是浅拷贝** `c := *t` —— 而它正是 Web UI 列表
// （`GET /api/cloud/tasks`）用的入口。浅拷贝**共享底层数组/指针**：
//   - `Chunks` 底层数组：`recordChunkState` 对同 index 是**原地改写** `task.Chunks[i] = st`；
//   - `*Diagnostics`：`recordKeptPartial` 在锁内写 `PartialPath` / `PartialSize`。
//
// HTTP 层在 `m.mu.RUnlock()` **之后**才 `json.Marshal` ⇒ 与后续的锁内写无同步 ⇒ DATA RACE。
//
// 测试方式：写面用**生产函数**（`recordChunkState` / 诊断写）并遵守生产锁纪律
// （`m.mu.Lock()` 内），读面复刻 HTTP handler（`ListTasks` 持锁 → 解锁后 `json.Marshal`）。
// 内建 HTTP 下载器**不报分块**（只有分块型下载器会），所以走真实下载无法触发该写面 —— 这是
// 本用例直接驱动生产写函数的原因（不是绕过实现，而是用同一份代码复刻同一时序）。

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestListTasks_SnapshotDeepCopyNoRace 并发「持锁写 Chunks/Diagnostics」与「ListTasks + 解锁后 marshal」。
func TestListTasks_SnapshotDeepCopyNoRace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(dir, "cap"), 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, actionsTestConfig())

	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-LIST-RACE-1", "list-race.bin", 4096, "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	live := mgr.tasks[task.ID]
	if live == nil {
		mgr.mu.Unlock()
		t.Fatalf("任务未注册: %s", task.ID)
	}
	// 预置一个 chunk，让写面走 recordChunkState 的**原地改写**分支（同 index 覆盖）。
	recordChunkState(live, downloader.ChunkState{Index: 0, Phase: "downloading"})
	mgr.mu.Unlock()

	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(2)
	// 写面：严格按生产锁纪律（m.mu.Lock 内写）。
	go func() {
		defer wg.Done()
		for n := range rounds {
			mgr.mu.Lock()
			recordChunkState(live, downloader.ChunkState{Index: 0, Phase: "done", Done: 1})
			// 诊断写：与 recordKeptPartial 同路径（它持 m.mu 写 PartialSize/PartialPath）。
			d := live.diag()
			d.PartialPath = "/tmp/partial"
			d.PartialSize = int64(n)
			mgr.mu.Unlock()
		}
	}()
	// 读面：**快照逃逸后延迟序列化**（这是浅拷贝真正的风险形态：HTTP 层拿到快照后
	// 由另一 goroutine/后续步骤序列化，期间下载 goroutine 继续持锁写同一底层数组）。
	go func() {
		defer wg.Done()
		var held []*CloudTask
		for range rounds {
			list, _ := mgr.ListTasks("", 0, 0, "")
			held = append(held, list...)
			if len(held) > 8 {
				_ = held[0].Chunks
				held = held[1:]
			}
		}
		// 延迟序列化（此时写面仍在跑）。
		for _, snap := range held {
			if _, merr := json.Marshal(snap); merr != nil {
				return
			}
		}
	}()
	wg.Wait()

	// 正向断言：快照的 Chunks 必须**不与活动任务共享底层数组**（深拷贝的直接证据；
	// 写面在并发期间原地改写会出现，故用写前的稳定状态检查）。
	list0, _ := mgr.ListTasks("", 0, 0, "")
	for _, snap := range list0 {
		if snap.ID != live.ID || len(snap.Chunks) == 0 {
			continue
		}
		if &snap.Chunks[0] == &live.Chunks[0] {
			t.Fatal("ListTasks 快照与活动任务共享 Chunks 底层数组（应为深拷贝）")
		}
	}
	// 语义断言：快照不应携带运行时配额句柄（与 SnapshotTask 一致）。
	list, _ := mgr.ListTasks("", 0, 0, "")
	for _, snap := range list {
		if snap.account != nil {
			t.Fatal("ListTasks 快照不应暴露运行时配额句柄")
		}
	}
}
