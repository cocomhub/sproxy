// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// transform_cache_gc_test.go 验证派生缓存 GC：
//  1. 孤儿 tmp（异常退出残留）被清理。
//  2. 过期键（TTL 外）被清理；新鲜键保留。
//  3. 清理幂等（重复调用安全）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// newCacheTenant 构造带 meta/transform 目录的租户。
func newCacheTenant(t *testing.T) *storage.Tenant {
	t.Helper()
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	if err := rt.MkdirAll("meta/transform", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tnt, terr := storage.NewTenant("t", rt)
	if terr != nil {
		t.Fatalf("NewTenant: %v", terr)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return tnt
}

// TestTransformCacheGC_RemovesOrphanTmp 孤儿 tmp 清理。
func TestTransformCacheGC_RemovesOrphanTmp(t *testing.T) {
	t.Parallel()
	tnt := newCacheTenant(t)
	dir, ok := tnt.Root().Abs("meta/transform")
	if !ok {
		t.Fatal("Abs 失败")
	}
	// 正常缓存文件 + 孤儿 tmp。
	key1 := transformCacheKey("a.png", "c1", 10, 100, "thumb", 128)
	if err := os.WriteFile(filepath.Join(dir, key1), []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, key1+".tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile tmp: %v", err)
	}

	CleanupTransformCache(tnt, TransformCacheGCOptions{})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("清理后文件数 = %d, want 1（孤儿 tmp 已删）", len(entries))
	}
	if entries[0].Name() != key1 {
		t.Fatalf("剩余文件 = %s, want %s（正常缓存保留）", entries[0].Name(), key1)
	}
}

// TestTransformCacheGC_RemovesExpired 过期键（TTL 外）删除；新鲜键保留。
func TestTransformCacheGC_RemovesExpired(t *testing.T) {
	t.Parallel()
	tnt := newCacheTenant(t)
	dir, ok := tnt.Root().Abs("meta/transform")
	if !ok {
		t.Fatal("Abs 失败")
	}
	keyOld := transformCacheKey("old.png", "c1", 10, 100, "thumb", 128)
	keyNew := transformCacheKey("new.png", "c2", 10, 200, "thumb", 128)
	pOld := filepath.Join(dir, keyOld)
	pNew := filepath.Join(dir, keyNew)
	if err := os.WriteFile(pOld, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile old: %v", err)
	}
	if err := os.WriteFile(pNew, []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile new: %v", err)
	}
	// 旧键 mtime 设为 TTL 外。
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(pOld, past, past); err != nil {
		t.Fatalf("Chtimes old: %v", err)
	}

	CleanupTransformCache(tnt, TransformCacheGCOptions{MaxAge: 7 * 24 * time.Hour})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("清理后文件数 = %d, want 1（过期键已删）", len(entries))
	}
	if entries[0].Name() != keyNew {
		t.Fatalf("剩余文件 = %s, want %s（新鲜键保留）", entries[0].Name(), keyNew)
	}
}

// TestTransformCacheGC_Idempotent 清理幂等。
func TestTransformCacheGC_Idempotent(t *testing.T) {
	t.Parallel()
	tnt := newCacheTenant(t)
	CleanupTransformCache(tnt, TransformCacheGCOptions{})
	CleanupTransformCache(tnt, TransformCacheGCOptions{})
	// 目录不存在的租户也安全。
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	tnt2, terr := storage.NewTenant("t2", rt)
	if terr != nil {
		t.Fatalf("NewTenant: %v", terr)
	}
	t.Cleanup(func() { _ = rt.Close() })
	CleanupTransformCache(tnt2, TransformCacheGCOptions{})
}
