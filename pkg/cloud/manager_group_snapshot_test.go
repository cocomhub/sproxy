// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"sync"
	"testing"
)

// TestGroupSnapshot_IsolatedFromTaskIDsMutation 锁定「组快照深拷贝 TaskIDs」：
// 快照会被 HTTP 层解锁后序列化，而组状态变更会在锁内改写 TaskIDs（2026-11-27 审查）。
func TestGroupSnapshot_IsolatedFromTaskIDsMutation(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{groups: map[string]*CloudTaskGroup{}}
	g := &CloudTaskGroup{ID: "g1", Name: "n", TaskIDs: []string{"t1"}}
	m.groups[g.ID] = g

	snap, ok := m.GetGroup(g.ID, "")
	if !ok {
		t.Fatal("组应存在")
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 2000 {
			m.groupMu.Lock()
			// 模拟组状态变更的**原地写**（append 在容量足够时也是原地写）：
			// 这正是浅拷贝快照会与写入共享数组的情形。
			g.TaskIDs[0] = "tx"
			m.groupMu.Unlock()
		}
	})
	for range 2000 {
		if len(snap.TaskIDs) > 0 {
			_ = snap.TaskIDs[0] // 读快照（模拟序列化）
		}
	}
	wg.Wait()
}

// TestListGroups_SnapshotIsolated 同上，覆盖 ListGroups 出口。
func TestListGroups_SnapshotIsolated(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{groups: map[string]*CloudTaskGroup{}}
	g := &CloudTaskGroup{ID: "g2", TaskIDs: []string{"a", "b"}}
	m.groups[g.ID] = g
	list, _ := m.ListGroups("", 0, 0, "")
	if len(list) != 1 {
		t.Fatalf("应返回 1 个组，got %d", len(list))
	}
	// 修改管理器内的组不应影响已返回的快照
	m.groupMu.Lock()
	g.TaskIDs = append(g.TaskIDs, "c")
	m.groupMu.Unlock()
	if len(list[0].TaskIDs) != 2 {
		t.Fatalf("快照应隔离，got %v", list[0].TaskIDs)
	}
}
