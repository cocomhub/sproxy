// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestVectorStore(t *testing.T) *VectorStore {
	t.Helper()
	return NewVectorStore(t.TempDir())
}

// TestVectorStore_PutGetDeleteRename 验证 map 生命周期：Put/Get/Delete/Rename 同步。
func TestVectorStore_PutGetDeleteRename(t *testing.T) {
	t.Parallel()
	vs := newTestVectorStore(t)
	vs.Put("ownerA", "a.txt", []float32{1, 0}, 100, "alpha")
	if e := vs.Get("ownerA", "a.txt"); e == nil || e.Vec[0] != 1 || e.Rev != 100 {
		t.Fatalf("Get = %+v, want Vec[0]=1 Rev=100", e)
	}
	// rename 同步
	vs.Rename("ownerA", "a.txt", "b.txt")
	if e := vs.Get("ownerA", "a.txt"); e != nil {
		t.Fatalf("rename 后旧 key 仍存在: %+v", e)
	}
	if e := vs.Get("ownerA", "b.txt"); e == nil || e.Text != "alpha" {
		t.Fatalf("rename 后新 key 缺失/内容错: %+v", e)
	}
	// delete 同步
	vs.Delete("ownerA", "b.txt")
	if e := vs.Get("ownerA", "b.txt"); e != nil {
		t.Fatalf("delete 后 key 仍存在: %+v", e)
	}
	// 不存在 key → nil（非 panic）
	if e := vs.Get("nobody", "x.txt"); e != nil {
		t.Fatalf("不存在 owner 应 nil: %+v", e)
	}
}

// TestVectorStore_PersistRoundtrip 验证落盘→载入一致。
func TestVectorStore_PersistRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vs := NewVectorStore(dir)
	vs.Put("ownerA", "a.txt", []float32{1, 0, 0}, 100, "alpha")
	vs.Put("ownerB", "dir/b.txt", []float32{0, 1, 0}, 200, "beta")
	if err := vs.SaveAll(); err != nil {
		t.Fatalf("SaveAll: %v", err)
	}
	vs2 := NewVectorStore(dir)
	vs2.LoadAll()
	if e := vs2.Get("ownerA", "a.txt"); e == nil || e.Vec[0] != 1 || e.Text != "alpha" {
		t.Fatalf("载入后 ownerA/a.txt = %+v", e)
	}
	if e := vs2.Get("ownerB", "dir/b.txt"); e == nil || e.Rev != 200 {
		t.Fatalf("载入后 ownerB/dir/b.txt = %+v", e)
	}
	// 损坏文件 → 忽略（幂等）
	bad := filepath.Join(dir, "ownerA.bin")
	os.WriteFile(bad, []byte("corrupt"), 0o644)
	vs3 := NewVectorStore(dir)
	vs3.LoadAll()
}

// TestVectorStore_Cosine 验证余弦相似度。
func TestVectorStore_Cosine(t *testing.T) {
	t.Parallel()
	a := []float32{1, 0}
	b := []float32{0, 1}
	if got := cosine(a, b); got != 0 {
		t.Fatalf("cosine(1,0),(0,1) = %f, want 0", got)
	}
	if got := cosine(a, a); got != 1 {
		t.Fatalf("cosine 自身 = %f, want 1", got)
	}
}
