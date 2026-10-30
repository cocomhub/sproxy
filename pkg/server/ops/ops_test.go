// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestWalkRoot_Recursive 递归统计目录/文件/字节，跳过 .__ 魔法目录与元数据文件。
func TestWalkRoot_Recursive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, p := range []string{"user/a.txt", "user/sub/b.txt", "user/.__trash/c.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte("12345"), 0o644); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
	}
	if wErr := os.WriteFile(filepath.Join(dir, "user", "checksums.json"), []byte("{}"), 0o644); wErr != nil {
		t.Fatalf("write meta: %v", wErr)
	}

	d, err := WalkRoot(root, "user")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if d.Path != "user" || d.Files != 2 || d.Dirs != 1 || d.Size != 10 {
		t.Fatalf("got %+v, want path=user files=2 dirs=1 size=10", d)
	}
}

// TestWalkFS_MatchesRoot 同一棵树经 FS 抽象与 Root 得到一致统计。
func TestWalkFS_MatchesRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if mkErr := os.MkdirAll(filepath.Join(dir, "user", "d"), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(dir, "user", "d", "f.bin"), []byte("abc"), 0o644); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}

	want, err := WalkRoot(root, "user")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	got, err := WalkFS(context.Background(), syncpkg.NewLocalFS(dir, slog.New(slog.DiscardHandler)), "user")
	if err != nil {
		t.Fatalf("WalkFS: %v", err)
	}
	if got.Files != want.Files || got.Dirs != want.Dirs || got.Size != want.Size {
		t.Fatalf("FS 与 Root 统计不一致: got %+v want %+v", got, want)
	}
}

// TestWalkUploadStats_CategorySplit 分类遍历：user/cloud/chunk/version 分桶、meta 与
// 内部目录跳过。
func TestWalkUploadStats_CategorySplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{
		"user/a.txt", "cloud/t/f.bin", "archive/t/f.bin",
		"chunk/s/0.part", "version/doc/v1", "meta/state.json",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte("1234"), 0o644); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
	}
	uf, ch, ve, cl := WalkUploadStatsByCategory(dir, slog.New(slog.DiscardHandler))
	if uf != 4 || cl != 8 || ch != 4 || ve != 4 {
		t.Fatalf("分类用量 = user=%d cloud=%d chunk=%d version=%d，want 4/8/4/4", uf, cl, ch, ve)
	}
	files, size := WalkUploadStats(dir, slog.New(slog.DiscardHandler))
	if files != 1 || size != 4 {
		t.Fatalf("WalkUploadStats = %d files / %d bytes，want 1/4（只统计 user 桶）", files, size)
	}
}

// TestCategoriesFromBuckets 聚合 path→bytes（末段桶名；cloud+archive 合并）。
func TestCategoriesFromBuckets(t *testing.T) {
	t.Parallel()
	uf, cl, ch, ve := CategoriesFromBuckets(map[string]int64{
		"alice/user": 10, "alice/cloud": 20, "alice/archive": 5,
		"alice/chunk": 3, "alice/version": 2, "alice/meta": 99,
	})
	if uf != 10 || cl != 25 || ch != 3 || ve != 2 {
		t.Fatalf("got %d/%d/%d/%d，want 10/25/3/2", uf, cl, ch, ve)
	}
}

// TestQuotaStatusOf 水位：nil / 不限 = 0；有限按百分比取整。
func TestQuotaStatusOf(t *testing.T) {
	t.Parallel()
	if got := QuotaStatusOf(nil); got != (QuotaStatus{}) {
		t.Fatalf("nil scope = %+v，want 零值", got)
	}
	sc := quota.NewPool(0).Scope("owner", 200)
	res, err := sc.TryReserve(50)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(50)
	got := QuotaStatusOf(sc)
	if got.Usage != 50 || got.MaxBytes != 200 || got.Watermark != 25 {
		t.Fatalf("got %+v，want usage=50 max=200 watermark=25", got)
	}
}
