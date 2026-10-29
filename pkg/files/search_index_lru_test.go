// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index_lru_test.go 钉住索引内存上限（审计 2026-10-29 P2-2）：
// `searchIndex` 此前对 owner 数无约束（每 owner 一份全量 map + contentTokens），多租户大库
// 内存随「被搜索过的 owner 数」线性增长。加 LRU 淘汰后：只保留最近使用的 N 个 owner，
// 被淘汰者下次访问按需重建（磁盘快照仍在，重建不打磁盘全量扫描）。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSearchIndex_OwnerLRUEvictsOldest：容量 2，访问 3 个 owner ⇒ 最旧的被淘汰；
// 再次访问它会重建（而非返回空）。
//
// 变异：不调 touchOwnerLocked ⇒ len(owners)==3 ⇒ 红。
func TestSearchIndex_OwnerLRUEvictsOldest(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	for _, owner := range []string{"u1", "u2", "u3"} {
		userAbs, ok := env.tenantFor(owner).Root().Abs("user")
		if !ok {
			t.Fatalf("Abs(user) for %s 失败", owner)
		}
		if err := os.MkdirAll(userAbs, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", owner, err)
		}
		if err := os.WriteFile(filepath.Join(userAbs, "f.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", owner, err)
		}
	}
	env.rebuild()
	svc := env.newService()
	if svc.index == nil {
		t.Fatal("index 未初始化")
	}
	svc.index.setMaxOwners(2)

	for _, owner := range []string{"u1", "u2", "u3"} {
		if res, err := svc.Search(SearchQuery{Owner: owner, Query: "f.txt"}); err != nil || len(res.Files) != 1 {
			t.Fatalf("search %s: n=%d err=%v", owner, len(res.Files), err)
		}
	}
	svc.index.mu.Lock()
	defer svc.index.mu.Unlock()
	if got := len(svc.index.owners); got != 2 {
		t.Fatalf("LRU 容量 2，内存索引 owner 数 = %d，want 2（未淘汰）", got)
	}
	if _, ok := svc.index.owners["u1"]; ok {
		t.Fatal("最旧的 u1 应被淘汰")
	}
	if _, ok := svc.index.owners["u3"]; !ok {
		t.Fatal("最近使用的 u3 应保留")
	}
}

// TestSearchIndex_OwnerEvictedRebuildsOnAccess：被淘汰的 owner 再次访问必须重新可见
// （重建而非返回空索引）。
func TestSearchIndex_OwnerEvictedRebuildsOnAccess(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.rebuild()
	svc := env.newService()
	svc.index.setMaxOwners(1)
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("首次搜索: n=%d err=%v", len(res.Files), err)
	}
	// 访问另一个 owner 触发淘汰（bob 需要租户可用）。
	if tnt := env.tenantFor("bob"); tnt != nil {
		if bobUser, bok := tnt.Root().Abs("user"); bok {
			_ = os.MkdirAll(bobUser, 0o755)
			_, _ = svc.Search(SearchQuery{Owner: "bob", Query: "zzz"})
		}
	}
	// alice 被淘汰后重建仍应命中。
	res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a.txt"})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("淘汰后重建应命中: n=%d err=%v", len(res.Files), err)
	}
}
