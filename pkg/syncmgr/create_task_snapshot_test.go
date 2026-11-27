// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

// create_task_snapshot_test.go 锁「CreateTask 去重命中返回的快照必须深拷贝」。
//
// 与 pkg/cloud 的「快照浅拷贝」缺陷族同源（R62 已把 cloud 侧机械化）：
// `SyncTask` 含**被原地改写**的切片/map ——
//   - `Results`：`backfillRetryResults` 在 **写锁内** 原地改写 `stored.Results[i].Action/Error/Checksum`；
//   - `Carriers`：执行结束回填（`task.Carriers = ...` 整体替换，但 map 仍可能被并发读）；
//   - `Include`/`Exclude`：创建期写入后只读，但同为引用类型。
// `CreateTask` 去重命中时 `c := *t` 后把 `&c` **直接返回给调用方**（HTTP 层随后解锁序列化）⇒
// 共享 `Results` 底层数组 ⇒ 调用方读到半更新条目 / 与写锁内改写竞争。
//
// 断言（确定性，不依赖时序）：快照的 `Results` 不得与活动任务共享底层数组。

import (
	"testing"
)

// TestCreateTask_DedupSnapshotDeepCopy 去重命中时快照不得共享 Results/Carriers。
func TestCreateTask_DedupSnapshotDeepCopy(t *testing.T) {
	t.Parallel()
	m := newTestManager(t, &mockQuota{}, nil, nil, &Config{})

	req := CreateRequest{
		Direction: string(DirectionPush),
		Remote:    "r1",
		Src:       "src-a",
		Dst:       "dst-a",
		Owner:     "demo",
	}
	first, _, err := m.CreateTask(req)
	if err != nil {
		t.Fatalf("首次 CreateTask: %v", err)
	}
	// 给活动任务塞入 Results，让"共享底层数组"可被观测。
	m.mu.Lock()
	stored := m.tasks[first.ID]
	if stored == nil {
		m.mu.Unlock()
		t.Fatalf("任务未注册: %s", first.ID)
	}
	stored.Results = []SyncFileResult{{Path: "a.txt", Action: "copied"}}
	stored.Carriers = map[string]int{"direct": 1}
	m.mu.Unlock()

	// 去重命中（同参同 owner）。
	snap, dup, serr := m.CreateTask(req)
	if serr != nil {
		t.Fatalf("去重 CreateTask: %v", serr)
	}
	if dup {
		t.Fatalf("同参请求应去重复用，不该新建任务")
	}
	if snap == nil || snap.ID != first.ID {
		t.Fatalf("去重应复用 %s，得到 %+v", first.ID, snap)
	}
	if len(snap.Results) == 0 {
		t.Fatalf("快照应含 Results")
	}
	// ① 确定性断言：不得共享底层数组。
	if &snap.Results[0] == &stored.Results[0] {
		t.Fatal("去重快照与活动任务共享 Results 底层数组（应为深拷贝）⇒ 调用方读到半更新条目")
	}
	// ② 写后不得影响已返回的快照（更强的语义断言）。
	m.mu.Lock()
	stored.Results[0].Action = "mutated"
	m.mu.Unlock()
	if snap.Results[0].Action != "copied" {
		t.Fatalf("活动任务改写后，快照被连带修改（%q）⇒ 共享状态", snap.Results[0].Action)
	}
	// ③ Carriers 同样不得共享。
	if len(snap.Carriers) > 0 {
		m.mu.Lock()
		stored.Carriers["direct"] = 99
		m.mu.Unlock()
		if snap.Carriers["direct"] == 99 {
			t.Fatal("去重快照与活动任务共享 Carriers map ⇒ 快照会随后续回填变化")
		}
	}
}
