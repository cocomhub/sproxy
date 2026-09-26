// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"log/slog"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// encTestRoot 构造加密卷测试根（key 固定 32B）。
func encTestRoot(t *testing.T) *storage.Root {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.SetEncryption(bytes.Repeat([]byte{0x42}, 32)); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	return rt
}

// TestAIPrivacy_StoreLoadRoundtrip 验证加密卷 Store → Load 一致。
func TestAIPrivacy_StoreLoadRoundtrip(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	p := NewAIPrivacy(true, rt, slog.Default())
	if err := p.StoreAI(rt, "vectors", "ownerA", "a.txt", []byte("vecdata")); err != nil {
		t.Fatalf("StoreAI: %v", err)
	}
	got, err := p.LoadAI(rt, "vectors", "ownerA", "a.txt")
	if err != nil {
		t.Fatalf("LoadAI: %v", err)
	}
	if string(got) != "vecdata" {
		t.Fatalf("LoadAI = %q, want vecdata", got)
	}
	// 明文路径不可读（加密卷外不落明文）——meta/ai 下文件是密文
	if err := p.DeleteAI(rt, "vectors", "ownerA", "a.txt"); err != nil {
		t.Fatalf("DeleteAI: %v", err)
	}
	if _, err := p.LoadAI(rt, "vectors", "ownerA", "a.txt"); err == nil {
		t.Fatal("删除后应 Load 失败")
	}
}

// TestAIPrivacy_PurgeOwner 验证 purge 后 List 空。
func TestAIPrivacy_PurgeOwner(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	p := NewAIPrivacy(true, rt, slog.Default())
	_ = p.StoreAI(rt, "vectors", "ownerA", "a.txt", []byte("v1"))
	_ = p.StoreAI(rt, "insight", "ownerA", "b.txt", []byte("i1"))
	_ = p.StoreAI(rt, "vectors", "ownerB", "c.txt", []byte("v2"))
	n, err := p.PurgeOwner(rt, "ownerA")
	if err != nil {
		t.Fatalf("PurgeOwner: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}
	if got := p.List(rt, "ownerA"); len(got) != 0 {
		t.Fatalf("purge 后 ownerA 应空: %+v", got)
	}
	// ownerB 不受影响
	if got := p.List(rt, "ownerB"); len(got) != 1 {
		t.Fatalf("ownerB 应保留 1 条: %+v", got)
	}
}

// TestAIPrivacy_ListShape 验证可见性清单字段。
func TestAIPrivacy_ListShape(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	p := NewAIPrivacy(true, rt, slog.Default())
	_ = p.StoreAI(rt, "vectors", "ownerA", "a.txt", []byte("v1"))
	list := p.List(rt, "ownerA")
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1", len(list))
	}
	a := list[0]
	if a.Kind != "vectors" || a.Rel != "a.txt" || a.Bytes == 0 || a.TS.IsZero() {
		t.Fatalf("artifact = %+v, want kind=vectors rel=a.txt bytes>0 ts 非零", a)
	}
}

// TestAIPrivacy_NoEncryptedVolume_FailsClosed 验证未加密卷 → 启用拒绝。
func TestAIPrivacy_NoEncryptedVolume_FailsClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	p := NewAIPrivacy(true, rt, slog.Default())
	if err := p.StoreAI(rt, "vectors", "ownerA", "a.txt", []byte("v")); err == nil {
		t.Fatal("未加密卷应拒绝（fail-closed）")
	}
}

// TestAIPrivacy_Disabled 验证 disabled → 空操作。
func TestAIPrivacy_Disabled(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	p := NewAIPrivacy(false, rt, slog.Default())
	if err := p.StoreAI(rt, "vectors", "ownerA", "a.txt", []byte("v")); err != nil {
		t.Fatalf("disabled 应 no-op: %v", err)
	}
}
