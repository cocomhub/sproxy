// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

// cache_budget_test.go 钉住内存上界口径（2026-10-30）：缓存除「目录数」外还有**总条目预算**
// ——单个 10 万文件的目录也能吃掉全部内存，故超预算的单目录快照直接不缓存。

import (
	"fmt"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func mkEntries(n int) []syncpkg.Entry {
	out := make([]syncpkg.Entry, 0, n)
	for i := range n {
		out = append(out, syncpkg.Entry{Name: fmt.Sprintf("f%d.txt", i), Size: 1})
	}
	return out
}

// TestCache_RejectsOversizedSnapshot：单目录 entries 超过总预算 ⇒ 不缓存（回源）。
//
// 变异：删掉 pushFront 的 `size > maxTotalEntries` 判断 ⇒ 本用例红。
func TestCache_RejectsOversizedSnapshot(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxEntries: 10, MaxTotalEntries: 5})
	key := cacheKey("vol", "huge")
	elem := c.pushFront(key, "vol", "huge", &DirSnapshot{Entries: mkEntries(6), Volume: "vol"})
	if elem != nil {
		t.Fatal("超过总预算的单目录快照不应被缓存")
	}
	if snap, _ := c.lookup(key); snap != nil {
		t.Fatal("超预算快照不应可命中")
	}
}

// TestCache_EvictsToTotalBudget：总条目超预算按 LRU 淘汰（目录数上限未触发）。
func TestCache_EvictsToTotalBudget(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxEntries: 100, MaxTotalEntries: 5})
	k1, k2 := cacheKey("vol", "a"), cacheKey("vol", "b")
	c.pushFront(k1, "vol", "a", &DirSnapshot{Entries: mkEntries(3), Volume: "vol"})
	c.pushFront(k2, "vol", "b", &DirSnapshot{Entries: mkEntries(3), Volume: "vol"})
	if snap, _ := c.lookup(k1); snap != nil {
		t.Fatal("总条目超预算应淘汰最旧目录快照")
	}
	if snap, _ := c.lookup(k2); snap == nil {
		t.Fatal("最新目录快照应保留")
	}
	if got := c.totalEntries; got != 3 {
		t.Fatalf("淘汰后总条目应为 3，got %d", got)
	}
}
