// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// insight_cache_lifecycle_test.go 补齐 InsightCache 的生命周期/健壮性分支：
//  1. 未知 owner / 未知 key → 未命中（无错误）；
//  2. owner 隔离；
//  3. 落盘快照损坏 → 忽略 + 可重建（不 panic）；
//  4. 非 .bin 文件/子目录在载入时跳过；
//  5. 落盘失败 → Put 返回错误（内存态仍可读）；
//  6. 缓存目录不可用 → 构造仍成功、Put 报错（fail-soft）；
//  7. 并发 Put/Get（-race 下验证）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestInsightCache_MissingOwnerAndKey 未知 owner/key → 未命中，且不返回错误。
func TestInsightCache_MissingOwnerAndKey(t *testing.T) {
	t.Parallel()
	c := NewInsightCache(filepath.Join(t.TempDir(), "meta"), nil) // nil logger → 默认 logger
	ctx := context.Background()

	if data, mtime, ok, err := c.Get(ctx, "ghost", "a.txt", "sum", 1); ok || err != nil || data != nil || mtime != 0 {
		t.Fatalf("未知 owner 应未命中: data=%q mtime=%d ok=%v err=%v", data, mtime, ok, err)
	}
	if err := c.Put(ctx, "alice", "a.txt", "sum", 1, []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, _, ok, err := c.Get(ctx, "alice", "b.txt", "sum", 1); ok || err != nil {
		t.Fatalf("未知 rel 应未命中: ok=%v err=%v", ok, err)
	}
}

// TestInsightCache_OwnerIsolation owner 之间同 (rel,kind) 互不可见。
func TestInsightCache_OwnerIsolation(t *testing.T) {
	t.Parallel()
	c := NewInsightCache(filepath.Join(t.TempDir(), "meta"), testAILogger())
	ctx := context.Background()
	if err := c.Put(ctx, "alice", "a.txt", "sum", 7, []byte("alice")); err != nil {
		t.Fatalf("Put(alice): %v", err)
	}
	if err := c.Put(ctx, "bob", "a.txt", "sum", 7, []byte("bob")); err != nil {
		t.Fatalf("Put(bob): %v", err)
	}
	for owner, want := range map[string]string{"alice": "alice", "bob": "bob"} {
		got, _, ok, err := c.Get(ctx, owner, "a.txt", "sum", 7)
		if err != nil || !ok || string(got) != want {
			t.Fatalf("owner %s 命中不符: data=%q ok=%v err=%v", owner, got, ok, err)
		}
	}
}

// TestInsightCache_CorruptedSnapshotIgnored 损坏快照 → Warn 忽略，缓存可重建。
func TestInsightCache_CorruptedSnapshotIgnored(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	insightDir := filepath.Join(base, "insight")
	if err := os.MkdirAll(insightDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(insightDir, "alice.bin"), []byte("not-gob"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := NewInsightCache(base, testAILogger())
	ctx := context.Background()
	if _, _, ok, err := c.Get(ctx, "alice", "a.txt", "sum", 1); ok || err != nil {
		t.Fatalf("损坏快照应忽略（未命中）: ok=%v err=%v", ok, err)
	}
	if err := c.Put(ctx, "alice", "a.txt", "sum", 1, []byte("rebuilt")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c2 := NewInsightCache(base, testAILogger())
	got, _, ok, err := c2.Get(ctx, "alice", "a.txt", "sum", 1)
	if err != nil || !ok || string(got) != "rebuilt" {
		t.Fatalf("重建后重载应命中: data=%q ok=%v err=%v", got, ok, err)
	}
}

// TestInsightCache_SkipsNonBinEntries 载入时跳过非 .bin 文件与子目录。
func TestInsightCache_SkipsNonBinEntries(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	insightDir := filepath.Join(base, "insight")
	if err := os.MkdirAll(filepath.Join(insightDir, "subdir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, name := range []string{"notes.txt", "owner.bak"} {
		if err := os.WriteFile(filepath.Join(insightDir, name), []byte("ignored"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}

	c := NewInsightCache(base, testAILogger())
	ctx := context.Background()
	if _, _, ok, err := c.Get(ctx, "notes", "a.txt", "sum", 1); ok || err != nil {
		t.Fatalf("非 .bin 条目不应载入: ok=%v err=%v", ok, err)
	}
	if err := c.Put(ctx, "alice", "a.txt", "sum", 2, []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// TestInsightCache_PersistFailure 落盘目标不可写 → Put 返回错误（内存态仍可读）。
func TestInsightCache_PersistFailure(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	c := NewInsightCache(base, testAILogger())
	// 把缓存目录替换成同名普通文件：<dir>/<owner>.bin 的写入必然失败。
	if err := os.Remove(c.dir); err != nil {
		t.Fatalf("Remove(dir): %v", err)
	}
	if err := os.WriteFile(c.dir, []byte("blocker"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocker): %v", err)
	}

	ctx := context.Background()
	err := c.Put(ctx, "alice", "a.txt", "sum", 3, []byte("v"))
	if err == nil {
		t.Fatal("落盘失败应返回错误")
	}
	if got, _, ok, gerr := c.Get(ctx, "alice", "a.txt", "sum", 3); gerr != nil || !ok || string(got) != "v" {
		t.Fatalf("落盘失败后内存态仍应命中: data=%q ok=%v err=%v", got, ok, gerr)
	}
}

// TestInsightCache_UnusableDir 缓存目录不可创建 → 构造成功、Put 报错（fail-soft）。
func TestInsightCache_UnusableDir(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "meta")
	if err := os.WriteFile(blocker, []byte("file"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewInsightCache(blocker, testAILogger())
	if c == nil {
		t.Fatal("目录不可用也应返回可用缓存实例")
	}
	if err := c.Put(context.Background(), "alice", "a.txt", "sum", 1, []byte("v")); err == nil {
		t.Fatal("目录不可用时 Put 应返回错误")
	}
}

// TestInsightCache_InternalMissingOwner 未登记的 owner：persistLocked/loadOwner 均为 no-op。
func TestInsightCache_InternalMissingOwner(t *testing.T) {
	t.Parallel()
	c := NewInsightCache(filepath.Join(t.TempDir(), "meta"), testAILogger())
	if err := c.persistLocked("ghost"); err != nil {
		t.Fatalf("未登记 owner 的落盘应为 no-op: %v", err)
	}
	c.loadOwner("ghost") // 落盘快照不存在 → 静默跳过
	if _, _, ok, err := c.Get(context.Background(), "ghost", "a.txt", "sum", 1); ok || err != nil {
		t.Fatalf("未登记 owner 应未命中: ok=%v err=%v", ok, err)
	}
}

// TestInsightCache_ConcurrentAccess 并发 Put/Get（-race 下验证锁与落盘串行化）。
func TestInsightCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := NewInsightCache(filepath.Join(t.TempDir(), "meta"), testAILogger())
	ctx := context.Background()

	const (
		writers   = 8
		perWriter = 20
	)
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func() {
			defer wg.Done()
			for i := range perWriter {
				rel := fmt.Sprintf("f%d.txt", w)
				mtime := int64(i + 1)
				if err := c.Put(ctx, "alice", rel, "sum", mtime, []byte(rel)); err != nil {
					t.Errorf("Put(%s,%d): %v", rel, mtime, err)
					return
				}
				got, gotMtime, ok, err := c.Get(ctx, "alice", rel, "sum", mtime)
				if err != nil || !ok || gotMtime != mtime || string(got) != rel {
					t.Errorf("Get(%s,%d): data=%q mtime=%d ok=%v err=%v", rel, mtime, got, gotMtime, ok, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
