// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync"
)

// newTestConflictIndex 构造指向 t.TempDir 的冲突索引（helper）。
func newTestConflictIndex(t *testing.T) *ConflictIndex {
	t.Helper()
	idx, err := NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// TestConflictIndex_RecordListGet 登记 + 列出 + 单条查询。
func TestConflictIndex_RecordListGet(t *testing.T) {
	t.Parallel()
	idx := newTestConflictIndex(t)
	rec := sync.ConflictRecord{
		Path: "a.txt", HunkCount: 1, BaseSHA: "b", OursSHA: "o", TheirsSHA: "t",
		Ours: []string{"o1"}, Theirs: []string{"t1"}, Timestamp: time.Now().UnixNano(),
	}
	idx.Record(rec)
	got := idx.List("")
	if len(got) != 1 || got[0].Path != "a.txt" {
		t.Fatalf("List = %+v, want 1 条 a.txt", got)
	}
	id := got[0].ID
	one, ok := idx.Get(id)
	if !ok || one.Path != "a.txt" {
		t.Fatalf("Get(%s) = %+v ok=%v", id, one, ok)
	}
}

// TestConflictIndex_RecordDedup 幂等去重：同 path+ts 不重复登记。
func TestConflictIndex_RecordDedup(t *testing.T) {
	t.Parallel()
	idx := newTestConflictIndex(t)
	ts := time.Now().UnixNano()
	rec := sync.ConflictRecord{Path: "a.txt", Timestamp: ts}
	idx.Record(rec)
	idx.Record(rec)
	if got := len(idx.List("")); got != 1 {
		t.Fatalf("去重后应 1 条, got %d", got)
	}
}

// TestConflictIndex_PersistReload 持久化重载：新索引读同 meta 目录可查历史。
func TestConflictIndex_PersistReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	idx1, err := NewConflictIndex(dir)
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	idx1.Record(sync.ConflictRecord{Path: "a.txt", Timestamp: time.Now().UnixNano()})
	if cerr := idx1.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	idx2, err := NewConflictIndex(dir)
	if err != nil {
		t.Fatalf("NewConflictIndex(2): %v", err)
	}
	defer idx2.Close()
	if got := len(idx2.List("")); got != 1 {
		t.Fatalf("重载后应 1 条, got %d", got)
	}
}

// TestConflictIndex_Resolve 解决：标 resolved + 内容写回（Ours 侧）。
func TestConflictIndex_Resolve(t *testing.T) {
	t.Parallel()
	idx := newTestConflictIndex(t)
	idx.Record(sync.ConflictRecord{
		Path: "a.txt", Ours: []string{"ours-content"}, Theirs: []string{"theirs-content"}, Timestamp: time.Now().UnixNano(),
	})
	items := idx.List("")
	if len(items) != 1 {
		t.Fatalf("预置失败: %d", len(items))
	}
	id := items[0].ID
	if _, err := idx.Resolve(id, "ours"); err != nil {
		t.Fatalf("Resolve(ours): %v", err)
	}
	// resolved 后不再列表出现。
	if got := len(idx.List("")); got != 0 {
		t.Fatalf("resolved 后应 0 条, got %d", got)
	}
}
