// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/state"
)

// TestIndexSyncLoop_ReloadOnChange 验证 Watch 推 put → 装配层调用 ReloadIndex。
func TestIndexSyncLoop_ReloadOnChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := state.NewLocalStateStore(dir, slog.Default(), state.WithPollInterval(50*time.Millisecond))
	svc := &fakeIndexService{applyRev: true}
	ctx := t.Context()
	loop := newIndexSyncLoop(ctx, st, svc, "index/", slog.Default())
	loop.Start() // 同步等待订阅成功

	// 主节点发布（写 StateStore）→ Watch 推 put → ReloadIndex 被调。
	env := files.NewIndexEnvelope(1, "node-1", map[string]files.IndexSnapshotEntryCompat{
		"a.txt": {Name: "a.txt", Base: "a"},
	})
	data, _ := json.Marshal(env)
	if err := st.Put(ctx, "index/ownerA", data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 条件轮询（不 time.Sleep 固定等待）。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if svc.reloaded.Load() > 0 {
			if svc.reloadedOwner() != "ownerA" {
				t.Fatalf("owner = %s, want ownerA", svc.reloadedOwner())
			}
			return
		}
		if svc.invalidatedCount() > 0 {
			t.Fatalf("不应 InvalidateIndex（Load 应成功）: %d", svc.invalidatedCount())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Watch put 后 ReloadIndex 未被调用")
}

// TestIndexSyncLoop_StaleRevIgnored 验证旧 rev 信封 → ReloadIndex 返回 false（不载入）。
func TestIndexSyncLoop_StaleRevIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := state.NewLocalStateStore(dir, slog.Default(), state.WithPollInterval(50*time.Millisecond))
	svc := &fakeIndexService{applyRev: true}
	ctx := t.Context()
	loop := newIndexSyncLoop(ctx, st, svc, "index/", slog.Default())
	loop.Start()

	// 先推 rev 2（应用）
	env2 := files.NewIndexEnvelope(2, "node-1", map[string]files.IndexSnapshotEntryCompat{
		"b.txt": {Name: "b.txt", Base: "b"},
	})
	data2, _ := json.Marshal(env2)
	_ = st.Put(ctx, "index/ownerA", data2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if svc.reloaded.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 再推 rev 1（旧）→ 不应触发 reload（applyRev=false 返回 false）
	svc.reloaded.Store(0)
	env1 := files.NewIndexEnvelope(1, "node-1", map[string]files.IndexSnapshotEntryCompat{
		"stale.txt": {Name: "stale.txt", Base: "stale"},
	})
	data1, _ := json.Marshal(env1)
	_ = st.Put(ctx, "index/ownerA", data1)
	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		if svc.reloaded.Load() > 0 {
			t.Fatal("旧 rev 不应触发 ReloadIndex")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fakeIndexService 是装配层探针（记录 ReloadIndex 调用）。
type fakeIndexService struct {
	reloaded        atomic.Int32
	invalidated     atomic.Int32
	lastOwner       atomic.Value
	lastInvalidated atomic.Value
	applyRev        bool // true = ReloadIndex 返回 true（模拟领域校验通过）
}

func (f *fakeIndexService) ReloadIndex(owner string, env *files.IndexEnvelope) bool {
	if f.applyRev {
		f.reloaded.Add(1)
		f.lastOwner.Store(owner)
		return true
	}
	// 模拟领域校验：旧 rev 返回 false（不记录 reload）
	return false
}

func (f *fakeIndexService) InvalidateIndex(owner string) {
	f.invalidated.Add(1)
	f.lastInvalidated.Store(owner)
}

func (f *fakeIndexService) invalidatedCount() int32 { return f.invalidated.Load() }

func (f *fakeIndexService) invalidatedOwner() string {
	if v := f.lastInvalidated.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (f *fakeIndexService) reloadedOwner() string {
	if v := f.lastOwner.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// TestIndexSyncLoop_ConsumeDirect 同步验证 consume 处理 Change（绕过 goroutine 调度）。
func TestIndexSyncLoop_ConsumeDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := state.NewLocalStateStore(dir, slog.Default())
	svc := &fakeIndexService{applyRev: true}
	ctx := context.Background()
	loop := newIndexSyncLoop(ctx, st, svc, "index/", slog.Default())
	// 预写 StateStore
	env := files.NewIndexEnvelope(1, "node-1", map[string]files.IndexSnapshotEntryCompat{
		"a.txt": {Name: "a.txt", Base: "a"},
	})
	data, _ := json.Marshal(env)
	_ = st.Put(ctx, "index/ownerA", data)
	ch := make(chan state.Change, 1)
	ch <- state.Change{Key: "index/ownerA", Op: "put"}
	close(ch)
	loop.consume(ch)
	if svc.reloaded.Load() == 0 {
		t.Fatal("consume 后 ReloadIndex 应被调用")
	}
	if svc.reloadedOwner() != "ownerA" {
		t.Fatalf("owner = %s, want ownerA", svc.reloadedOwner())
	}
}

// TestResyncLoop_CatchesUp 验证落后 owner 被 resync 重载。
func TestResyncLoop_CatchesUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := state.NewLocalStateStore(dir, slog.Default())
	svc := &fakeIndexService{applyRev: true}
	ctx := t.Context()
	// 预写快照（rev 1）
	env := files.NewIndexEnvelope(1, "node-1", map[string]files.IndexSnapshotEntryCompat{
		"c.txt": {Name: "c.txt", Base: "c"},
	})
	data, _ := json.Marshal(env)
	_ = st.Put(ctx, "index/ownerX", data)
	loop := newResyncLoop(ctx, st, svc, "index/", 30*time.Millisecond, slog.Default())
	loop.Start()
	defer loop.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if svc.reloaded.Load() > 0 {
			if svc.reloadedOwner() != "ownerX" {
				t.Fatalf("owner = %s, want ownerX", svc.reloadedOwner())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("resync 后 ReloadIndex 未被调用")
}
