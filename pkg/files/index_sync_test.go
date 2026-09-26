// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"encoding/json"
	"testing"
)

// TestIndexEnvelope_RoundTrip 验证信封 JSON 序列化 + DTO 互转。
func TestIndexEnvelope_RoundTrip(t *testing.T) {
	t.Parallel()
	env := &indexEnvelope{
		Rev: 3, Node: "node-1", Updated: 123,
		Entries: toSnapshotEntries(map[string]*indexEntry{
			"a.txt": {name: "a.txt", base: "a", size: 10, modTime: 100},
		}),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back indexEnvelope
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Rev != 3 || back.Node != "node-1" || len(back.Entries) != 1 {
		t.Fatalf("back = %+v", back)
	}
}

// TestReloadIndex_AppliesNewerRev 验证 rev 递增载入 → 命中新条目。
func TestReloadIndex_AppliesNewerRev(t *testing.T) {
	t.Parallel()
	ix := newSearchIndex(nil, nil, nil, nil, false)
	// 先建 owner 索引（rev 0）
	ix.ensureOwnerIndex("ownerA")
	env := &indexEnvelope{Rev: 1, Node: "n1", Entries: toSnapshotEntries(map[string]*indexEntry{
		"new.txt": {name: "new.txt", base: "new", size: 5, modTime: 50},
	})}
	if ok := ix.ReloadIndex("ownerA", env); !ok {
		t.Fatal("rev 1 > 0 应载入")
	}
	if e := ix.getEntry("ownerA", "new.txt"); e == nil || e.size != 5 {
		t.Fatalf("载入后应命中 new.txt: %+v", e)
	}
}

// TestReloadIndex_StaleRevIgnored 验证旧 rev 忽略（不覆盖）。
func TestReloadIndex_StaleRevIgnored(t *testing.T) {
	t.Parallel()
	ix := newSearchIndex(nil, nil, nil, nil, false)
	ix.ensureOwnerIndex("ownerA")
	env1 := &indexEnvelope{Rev: 2, Node: "n1", Entries: toSnapshotEntries(map[string]*indexEntry{
		"b.txt": {name: "b.txt", base: "b", size: 7, modTime: 70},
	})}
	ix.ReloadIndex("ownerA", env1)
	env2 := &indexEnvelope{Rev: 1, Node: "n1", Entries: toSnapshotEntries(map[string]*indexEntry{
		"stale.txt": {name: "stale.txt", base: "stale", size: 1, modTime: 1},
	})}
	if ok := ix.ReloadIndex("ownerA", env2); ok {
		t.Fatal("旧 rev 应忽略")
	}
	if e := ix.getEntry("ownerA", "b.txt"); e == nil {
		t.Fatal("旧 rev 不应覆盖（b.txt 应仍在）")
	}
	if e := ix.getEntry("ownerA", "stale.txt"); e != nil {
		t.Fatal("旧 rev 不应载入 stale.txt")
	}
}

// TestReloadIndex_CorruptFallbackToRebuild 验证损坏（nil entries）→ 回退重建。
func TestReloadIndex_CorruptFallbackToRebuild(t *testing.T) {
	t.Parallel()
	ix := newSearchIndex(nil, nil, nil, nil, false)
	ix.ensureOwnerIndex("ownerA")
	// 损坏信封（Entries nil）→ ReloadIndex 返回 false（调用方 InvalidateIndex）
	env := &indexEnvelope{Rev: 5, Node: "n1", Entries: nil}
	if ok := ix.ReloadIndex("ownerA", env); ok {
		t.Fatal("损坏信封应返回 false（回退重建）")
	}
}

// TestAttachIndexSync_NilNoop 验证未装配 sync 时 publishDirty no-op（零回归）。
func TestAttachIndexSync_NilNoop(t *testing.T) {
	t.Parallel()
	ix := newSearchIndex(nil, nil, nil, nil, false)
	ix.AttachIndexSync(nil) // 应 no-op
	ix.ensureOwnerIndex("ownerA")
	ix.markDirty("ownerA")
	if n := ix.publishDirty(context.Background()); n != 0 {
		t.Fatalf("未装配 sync → publishDirty = %d, want 0（零回归）", n)
	}
}

// TestPublish_DirtyTracking 验证写路径增量置 dirty → Publish 只发 dirty owner。
func TestPublish_DirtyTracking(t *testing.T) {
	t.Parallel()
	ix := newSearchIndex(nil, nil, nil, nil, false)
	mock := &mockIndexSync{}
	ix.AttachIndexSync(mock)
	ix.ensureOwnerIndex("ownerA")
	ix.ensureOwnerIndex("ownerB") // ownerB 已构建但未 dirty → 不 Publish
	ix.markDirty("ownerA")
	ix.publishDirty(context.Background())
	if mock.published != 1 {
		t.Fatalf("published = %d, want 1（只 dirty 已构建 owner）", mock.published)
	}
	if mock.owners[0] != "ownerA" {
		t.Fatalf("owners = %v, want [ownerA]", mock.owners)
	}
}

// mockIndexSync 是 IndexSync 测试桩。
type mockIndexSync struct {
	published int
	owners    []string
}

func (m *mockIndexSync) Publish(ctx context.Context, owner string, entries map[string]*IndexSnapshotEntry) error {
	m.published++
	m.owners = append(m.owners, owner)
	return nil
}

func (m *mockIndexSync) Load(ctx context.Context, owner string) (*indexEnvelope, error) {
	return nil, nil
}
