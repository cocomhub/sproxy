// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// index_persist_test.go 验证搜索索引持久化（roadmap 2.3 P0 增强）：
//  1. saveIndexSnapshot → 落盘 <meta>/index/<owner>.json（原子写）。
//  2. loadIndexSnapshot → 载入返回 entries（免全量 WalkDir）。
//  3. 损坏/缺失快照 → (nil, false)（回退全量重建，不报错）。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIndexSnapshot_RoundTrip 快照落盘 + 载入往返。
func TestIndexSnapshot_RoundTrip(t *testing.T) {
	t.Parallel()
	tnt := newCacheTenant(t)
	entries := map[string]*indexEntry{
		"dir/a.txt": {name: "dir/a.txt", base: "a.txt", size: 10, modTime: 100},
		"b.png":     {name: "b.png", base: "b.png", size: 20, modTime: 200},
	}
	owner := "alice"
	if err := saveIndexSnapshot(tnt, owner, entries); err != nil {
		t.Fatalf("saveIndexSnapshot: %v", err)
	}

	got, ok := loadIndexSnapshot(tnt, owner)
	if !ok {
		t.Fatal("loadIndexSnapshot 应命中")
	}
	if len(got) != 2 {
		t.Fatalf("载入 entries 数 = %d, want 2", len(got))
	}
	e, ok := got["dir/a.txt"]
	if !ok {
		t.Fatal("缺 dir/a.txt")
	}
	if e.name != "dir/a.txt" || e.base != "a.txt" || e.size != 10 || e.modTime != 100 {
		t.Fatalf("条目字段不完整: %+v", e)
	}
}

// TestEnsureOwner_SnapshotLoadFallback 损坏快照 → ensureOwner 回退全量重建（不 panic）。
func TestEnsureOwner_SnapshotLoadFallback(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	// 写损坏快照。
	tnt := env.tenantFor("alice")
	dir, ok := tnt.Root().Abs("meta/index")
	if !ok {
		t.Fatal("Abs(meta/index) 失败")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alice.json"), []byte("bad"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 上传文件（构建源）。
	userAbs, ok := tnt.Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("MkdirAll(user): %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 搜索触发 ensureOwner：损坏快照 → 全量重建（命中 x.txt）。
	svc := env.newService()
	if n, err := svc.Search(SearchQuery{Owner: "alice", Query: "x.txt"}); err != nil || len(n.Files) != 1 {
		t.Fatalf("损坏快照应回退重建, hits=%d err=%v", len(n.Files), err)
	}
}

// TestIndexSnapshot_MissingCorrupt 缺失/损坏快照 → (nil, false) 回退重建。
func TestIndexSnapshot_MissingCorrupt(t *testing.T) {
	t.Parallel()
	tnt := newCacheTenant(t)

	// 缺失 → false。
	if _, ok := loadIndexSnapshot(tnt, "nobody"); ok {
		t.Fatal("缺失快照应返回 false")
	}

	// 损坏 → false。
	dir, ok := tnt.Root().Abs("meta/index")
	if !ok {
		t.Fatal("Abs(meta/index) 失败")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alice.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, ok := loadIndexSnapshot(tnt, "alice"); ok {
		t.Fatal("损坏快照应返回 false（回退全量重建）")
	}
}
