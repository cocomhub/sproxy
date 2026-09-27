// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// state_dedup_test.go 验证去重台账的 StateStore 适配（statestore.md §5.1 P0 /
// cluster-state-migration.md §2.2）：
//   - 适配器往返：Add/RemoveRef/Rename/DeletePrefix/RefCount/FirstRel 全流程经 StateStore
//     落盘与重载还原（**既有 dedup_test.go 行为套件在新后端上不改断言全绿**——关键验收）；
//   - 双读单写：StateStore 未命中回退读旧 <meta>/dedup.json；首写恒写 StateStore 新路径
//     （旧 meta 不再改写）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// writeLegacyDedupFile 在 metaDir 写旧 <meta>/dedup.json（entries map JSON 格式）。
func writeLegacyDedupFile(t *testing.T, metaDir string, entries map[string]*dedupEntry) string {
	t.Helper()
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(metaDir, "dedup.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stateDedupStoreFor 构造 StateStore 后端的 DedupStore（t.TempDir 隔离）。
func stateDedupStoreFor(t *testing.T, legacyPath string) (*DedupStore, *state.LocalStateStore) {
	t.Helper()
	st := state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
	ds := NewDedupStore(filepath.Join(t.TempDir(), "dedup.json"), testLogger(), &DedupStateOptions{
		St:         st,
		Key:        "dedup/alice/all",
		LegacyPath: legacyPath,
	})
	return ds, st
}

// TestDedupStore_StateAdapter_PersistAndLoad 验证 StateStore 后端往返：Add 引用 + 查重 +
// 重载后一致（与 TestDedupStore_PersistAndLoad 同断言，跑在新后端上）。
func TestDedupStore_StateAdapter_PersistAndLoad(t *testing.T) {
	t.Parallel()
	ds, _ := stateDedupStoreFor(t, "")

	if !ds.Add("user/a.txt", "vol0", "abc123") {
		t.Fatal("首个引用 Add 应返回 true（新 checksum）")
	}
	if ds.Add("user/b.txt", "vol0", "abc123") {
		t.Fatal("同 checksum 第二引用 Add 应返回 false（已存在首份）")
	}
	rel, ok := ds.FirstRel("vol0", "abc123")
	if !ok || rel != "user/a.txt" {
		t.Fatalf("FirstRel(vol0, abc123)=%q,%v want user/a.txt,true", rel, ok)
	}
	if ds.RefCount("abc123") != 2 {
		t.Fatalf("RefCount(abc123)=%d want 2", ds.RefCount("abc123"))
	}

	// 落盘重载（新实例从 StateStore 读回）。
	ds2 := NewDedupStore(filepath.Join(t.TempDir(), "dedup.json"), testLogger(), &DedupStateOptions{
		St:         ds.state.st,
		Key:        "dedup/alice/all",
		LegacyPath: "",
	})
	if ds2.RefCount("abc123") != 2 {
		t.Fatalf("重载后 RefCount(abc123)=%d want 2", ds2.RefCount("abc123"))
	}
	if _, ok := ds2.FirstRel("vol0", "abc123"); !ok {
		t.Fatal("重载后 FirstRel 应命中")
	}

	// 摘引用。
	ds2.RemoveRef("user/a.txt", "vol0", "abc123")
	if ds2.RefCount("abc123") != 1 {
		t.Fatalf("摘引用后 RefCount(abc123)=%d want 1", ds2.RefCount("abc123"))
	}
	ds2.RemoveRef("user/b.txt", "vol0", "abc123")
	if ds2.RefCount("abc123") != 0 {
		t.Fatalf("全部摘除后 RefCount(abc123)=%d want 0", ds2.RefCount("abc123"))
	}
	if _, ok := ds2.FirstRel("vol0", "abc123"); ok {
		t.Fatal("全部摘除后 FirstRel 不应命中")
	}
}

// TestDedupStore_StateAdapter_Rename 验证 StateStore 后端重命名后台账引用路径更新。
func TestDedupStore_StateAdapter_Rename(t *testing.T) {
	t.Parallel()
	ds, _ := stateDedupStoreFor(t, "")
	ds.Add("user/a.txt", "vol0", "abc123")
	ds.Add("user/b.txt", "vol0", "abc123")
	ds.Rename("user/a.txt", "user/c.txt")
	if _, ok := ds.RelExists("user/c.txt", "vol0", "abc123"); !ok {
		t.Fatal("Rename 后 user/c.txt 应在引用列表")
	}
	if _, ok := ds.RelExists("user/a.txt", "vol0", "abc123"); ok {
		t.Fatal("Rename 后 user/a.txt 不应在引用列表")
	}
	if ds.RefCount("abc123") != 2 {
		t.Fatalf("Rename 后 RefCount=%d want 2", ds.RefCount("abc123"))
	}
}

// TestDedupStore_StateAdapter_LegacyFallback 验证双读：StateStore 未命中 → 回退读旧
// <meta>/dedup.json 可载入（迁移前存量零丢失）。
func TestDedupStore_StateAdapter_LegacyFallback(t *testing.T) {
	t.Parallel()
	legacyPath := writeLegacyDedupFile(t, filepath.Join(t.TempDir(), "anonymous", "meta"), map[string]*dedupEntry{
		"abc123": {Refs: []dedupRef{
			{Volume: "vol0", Rel: "user/a.txt"},
			{Volume: "vol0", Rel: "user/b.txt"},
		}},
	})
	ds, _ := stateDedupStoreFor(t, legacyPath)
	if got := ds.RefCount("abc123"); got != 2 {
		t.Fatalf("回退载入 RefCount(abc123)=%d want 2", got)
	}
	if rel, ok := ds.FirstRel("vol0", "abc123"); !ok || rel != "user/a.txt" {
		t.Fatalf("回退载入 FirstRel=%q,%v", rel, ok)
	}
}

// TestDedupStore_StateAdapter_SaveMigratesToState 验证单写：回退载入后首次写恒写
// StateStore 新路径（迁移发生，旧 meta 文件不被改写）。
func TestDedupStore_StateAdapter_SaveMigratesToState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	metaDir := filepath.Join(t.TempDir(), "anonymous", "meta")
	legacyPath := writeLegacyDedupFile(t, metaDir, map[string]*dedupEntry{
		"abc123": {Refs: []dedupRef{{Volume: "vol0", Rel: "user/a.txt"}}},
	})
	legacyRaw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	ds, st := stateDedupStoreFor(t, legacyPath)
	if ds.RefCount("abc123") != 1 {
		t.Fatal("回退载入应命中")
	}
	// 首写 → StateStore；旧 meta 文件保持原内容不被改写（单写）。
	ds.Add("user/b.txt", "vol0", "abc123")
	if _, gerr := st.Get(ctx, "dedup/alice/all"); gerr != nil {
		t.Fatalf("首写应写 StateStore 新路径: %v", gerr)
	}
	cur, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != string(legacyRaw) {
		t.Fatalf("旧 meta 文件不应被改写（单写语义）")
	}
}
