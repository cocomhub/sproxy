// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// state_index_test.go 验证搜索索引快照的 StateStore 适配（statestore.md §5.1 P1 /
// cluster-state-migration.md §3.3）：
//   - stateBackedIndexStore：index/<owner> 单 key——save 委托 StateStore.Put（快照覆盖），
//     load 委托 Get（未命中 → 空索引 false）；
//   - 双读单写：StateStore 未命中回退读旧 <meta>/index/<owner>.json（迁移前存量零丢失）；
//     首写恒写 StateStore 新路径（旧 meta 不再改写）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// writeLegacyIndexFile 在 metaDir 写旧 <meta>/index/<owner>.json（indexSnapshotFile 格式）。
func writeLegacyIndexFile(t *testing.T, metaDir, owner string, entries map[string]*indexEntry) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(metaDir, "index"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(indexSnapshotFile{Entries: toSnapshotEntries(entries)})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(metaDir, "index", owner+".json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStateBackedIndexStore_RoundTrip 验证单 key 快照往返：save 落 StateStore（index/<owner>）
// → load 读回（未命中 → 空索引 false）。
func TestStateBackedIndexStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	idx := newStateBackedIndexStore(st, filepath.Join(t.TempDir(), "missing", "index"))

	entries := map[string]*indexEntry{
		"dir/a.txt": {name: "dir/a.txt", base: "a.txt", size: 10, modTime: 100},
		"b.png":     {name: "b.png", base: "b.png", size: 20, modTime: 200},
	}
	if err := idx.save("alice", entries); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, gerr := st.Get(ctx, "index/alice"); gerr != nil {
		t.Fatalf("save 应落 StateStore 新路径: %v", gerr)
	}
	got, ok := idx.load("alice")
	if !ok {
		t.Fatal("load 应命中")
	}
	if len(got) != 2 || got["dir/a.txt"].name != "dir/a.txt" || got["b.png"].size != 20 {
		t.Fatalf("载入 entries 不符: %+v", got)
	}
	// 未命中 owner → (nil, false)（调用方全量重建）。
	if _, ok := idx.load("nobody"); ok {
		t.Fatal("未命中 owner 应返回 false（空索引）")
	}
}

// TestStateBackedIndexStore_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/index/<owner>.json 可载入（迁移前存量零丢失）。
func TestStateBackedIndexStore_LegacyFallback(t *testing.T) {
	t.Parallel()
	metaDir := filepath.Join(t.TempDir(), "alice", "meta")
	writeLegacyIndexFile(t, metaDir, "alice", map[string]*indexEntry{
		"legacy.txt": {name: "legacy.txt", base: "legacy.txt", size: 7},
	})

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	idx := newStateBackedIndexStore(st, filepath.Join(metaDir, "index"))
	got, ok := idx.load("alice")
	if !ok {
		t.Fatal("回退读旧文件应命中")
	}
	if e, found := got["legacy.txt"]; !found || e.size != 7 {
		t.Fatalf("回退载入内容不符: %+v", got)
	}
}

// TestStateBackedIndexStore_SaveMigratesToState 验证单写：回退载入后首写恒写 StateStore
// 新路径（迁移发生，旧 meta 文件不被改写）。
func TestStateBackedIndexStore_SaveMigratesToState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	metaDir := filepath.Join(t.TempDir(), "alice", "meta")
	legacyPath := writeLegacyIndexFile(t, metaDir, "alice", map[string]*indexEntry{
		"legacy.txt": {name: "legacy.txt", base: "legacy.txt", size: 7},
	})
	legacyRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	idx := newStateBackedIndexStore(st, filepath.Join(metaDir, "index"))
	if _, ok := idx.load("alice"); !ok {
		t.Fatal("回退载入应命中")
	}
	// 首写 → StateStore；旧 meta 文件保持原内容不被改写（单写）。
	if serr := idx.save("alice", map[string]*indexEntry{"new.txt": {name: "new.txt", base: "new.txt"}}); serr != nil {
		t.Fatal(serr)
	}
	if _, gerr := st.Get(ctx, "index/alice"); gerr != nil {
		t.Fatalf("首写应写 StateStore 新路径: %v", gerr)
	}
	cur, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != string(legacyRaw) {
		t.Fatal("旧 meta 文件不应被改写（单写语义）")
	}
}
