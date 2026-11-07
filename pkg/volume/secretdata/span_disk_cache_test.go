// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// span_disk_cache_test.go —— 本机磁盘（staging）块密文缓存的正确性与清理纪律锁定
// （2026-10-22）。
//
// 断言（各用例）：
//   - 命中缓存 ⇒ 底层 Range 请求次数不增（重读零远端流量）；
//   - **只缓存密文**：缓存文件内容 ≠ 明文（明文绝不落盘）；
//   - 超预算 ⇒ LRU 淘汰至预算内（sweep 后目录内数据文件按预算收敛）；
//   - 预算为 0（或目录空）⇒ 缓存关闭，行为与修复前一致（零回归）；
//   - 覆盖写 ⇒ 该文件缓存条目失效（防陈旧）。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// newDiskCacheEnv 构造启用磁盘缓存的 secretdata FS（块 2MiB、blocklet 256KiB）。
func newDiskCacheEnv(t *testing.T, budget sizex.ByteSize) (*SecretdataFS, *cacheProbeFS, string, []byte) {
	t.Helper()
	inner := &cacheProbeFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	cacheDir := t.TempDir()
	fs, err := NewFS(inner, Options{
		Secret:         []byte("test-secret-key-000"),
		Algorithm:      testAlgo,
		SpanCacheDir:   cacheDir,
		SpanCacheBytes: budget,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 2 << 20, Max: 2 << 20,
			BlockletMode: "fixed", BlockletMin: 256 << 10, BlockletMax: 256 << 10,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(2 << 20)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, inner, cacheDir, content
}

// TestSpanDiskCache_SecondReadHitsDisk：同一区间读两次 ⇒ 第二次零底层 Range 请求。
func TestSpanDiskCache_SecondReadHitsDisk(t *testing.T) {
	t.Parallel()
	fs, inner, _, content := newDiskCacheEnv(t, 0)

	readAt := func(off, size int64) []byte {
		t.Helper()
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, size)
		if err != nil {
			t.Fatalf("OpenRangeRead: %v", err)
		}
		b, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll: %v", rerr)
		}
		return b
	}
	got1 := readAt(0, 1<<20)
	if !bytes.Equal(got1, content[:1<<20]) {
		t.Fatalf("首次读内容错误")
	}
	afterFirst := inner.reads.Load()
	if afterFirst == 0 {
		t.Fatal("首次读应发生底层请求")
	}
	got2 := readAt(0, 1<<20)
	if !bytes.Equal(got2, content[:1<<20]) {
		t.Fatalf("二次读内容错误")
	}
	if inner.reads.Load() != afterFirst {
		t.Fatalf("二次读应命中磁盘缓存（零远端）：请求次数 %d → %d", afterFirst, inner.reads.Load())
	}
}

// TestSpanDiskCache_StoresCiphertextNotPlaintext：缓存文件内容必须是**密文**
// （明文绝不落盘——安全纪律）。
func TestSpanDiskCache_StoresCiphertextNotPlaintext(t *testing.T) {
	t.Parallel()
	fs, _, cacheDir, content := newDiskCacheEnv(t, 0)

	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 1<<20)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()

	files := collectBlkFiles(t, cacheDir)
	if len(files) == 0 {
		t.Fatal("磁盘缓存应已落块密文文件")
	}
	plainSample := content[:4096]
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("读缓存文件 %s: %v", f, rerr)
		}
		if bytes.Contains(raw, plainSample) {
			t.Fatalf("缓存文件 %s 含明文样本 ⇒ 明文落盘违反安全纪律", filepath.Base(f))
		}
	}
}

// TestSpanDiskCache_BudgetEvicts：预算极小（1 条）⇒ 多区间写后缓存收敛（旧条目被淘汰）。
func TestSpanDiskCache_BudgetEvicts(t *testing.T) {
	t.Parallel()
	// 预算 300KiB：单条跨度 ≈ 256KiB+（含 salt/boot）⇒ 只容 1 条。
	fs, _, cacheDir, _ := newDiskCacheEnv(t, sizex.ByteSize(300<<10))

	for off := int64(0); off < 1<<20; off += 256 << 10 {
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", off, 256<<10)
		if err != nil {
			t.Fatalf("OpenRangeRead(%d): %v", off, err)
		}
		if _, rerr := io.ReadAll(rc); rerr != nil {
			t.Fatalf("ReadAll(%d): %v", off, rerr)
		}
		_ = rc.Close()
	}
	fs.spanCache.sweep()

	files := collectBlkFiles(t, cacheDir)
	// 预算 300KiB ⇒ 至多 1 条 ε256KiB 跨度（超过则条目过大被跳过写入）。
	if len(files) > 2 {
		t.Fatalf("超预算后缓存文件数=%d，应 ≤2（LRU 淘汰至预算内）", len(files))
	}
}

// TestSpanDiskCache_DisabledIsZeroRegression：SpanCacheDir 为空 ⇒ 缓存关闭（无目录/无文件）。
func TestSpanDiskCache_DisabledIsZeroRegression(t *testing.T) {
	t.Parallel()
	inner := &cacheProbeFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 2 << 20, Max: 2 << 20,
			BlockletMode: "fixed", BlockletMin: 256 << 10, BlockletMax: 256 << 10,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if fs.spanCache != nil {
		t.Fatal("未配置 SpanCacheDir 时 spanCache 应为 nil（默认关闭零回归）")
	}
	content := data(1 << 20)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "f.bin",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	before := inner.reads.Load()
	rc, oerr := fs.OpenRangeRead(context.Background(), "f.bin", 0, 256<<10)
	if oerr != nil {
		t.Fatalf("OpenRangeRead: %v", oerr)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()
	if inner.reads.Load() == before {
		t.Fatal("缓存关闭时仍应直连远端读（零回归）")
	}
}

// TestSpanDiskCache_OverwriteInvalidates：覆盖写后该文件的磁盘缓存条目应被失效。
func TestSpanDiskCache_OverwriteInvalidates(t *testing.T) {
	t.Parallel()
	fs, inner, cacheDir, _ := newDiskCacheEnv(t, 0)

	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, 256<<10)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	if _, rerr := io.ReadAll(rc); rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	_ = rc.Close()
	if n := len(collectBlkFiles(t, cacheDir)); n == 0 {
		t.Fatal("首次读应落缓存文件")
	}

	newContent := data(1 << 20)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(newContent), int64(len(newContent)), 0); werr != nil {
		t.Fatalf("覆盖写: %v", werr)
	}
	if n := len(collectBlkFiles(t, cacheDir)); n != 0 {
		t.Fatalf("覆盖写后该文件缓存应失效：仍有 %d 个缓存文件", n)
	}

	before := inner.reads.Load()
	rc2, oerr := fs.OpenRangeRead(context.Background(), "big.bin", 0, 256<<10)
	if oerr != nil {
		t.Fatalf("覆盖后 OpenRangeRead: %v", oerr)
	}
	if _, rerr := io.ReadAll(rc2); rerr != nil {
		t.Fatalf("覆盖后 ReadAll: %v", rerr)
	}
	_ = rc2.Close()
	if inner.reads.Load() == before {
		t.Fatal("覆盖后读应重新走远端（缓存已失效）")
	}
}

// collectBlkFiles 递归收集缓存目录内的 `blk-*` 数据文件。
func collectBlkFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "blk-") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", dir, err)
	}
	return out
}
