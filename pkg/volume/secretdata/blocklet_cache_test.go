// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// blocklet_cache_test.go —— 块级明文 LRU 缓存的正确性锁定（2026-10-22 缺陷修复）。
//
// 背景（真机实测）：RangeSeeker 外部卷窗口 256KiB « 视频 GOP blocklet（可达 10MB）⇒
// 无缓存时同一块的多个窗口各自重拉整段密文（10MB 顺序读曾发 120 次 Range GET，且窗口
// 超时中断使下载内容错位）。缓存把「窗口读」与「网络读」解耦：同一块只取一次密文、只解
// 一次，后续窗口直接按明文偏移切。
//
// 断言（各用例）：
//   - 多窗口读同一块 ⇒ 底层 Range 请求次数 = 1（而非窗口数）；
//   - 内容仍与源逐字节一致（切偏移正确）；
//   - 覆盖写后缓存失效（旧明文不被后续读命中）；
//   - 关闭缓存（负预算）⇒ 零回归（行为与修复前一致：每窗口一次请求）。

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// cacheProbeFS 记录底层 OpenRangeRead/OpenRead 次数（缓存命中数断言用）。
type cacheProbeFS struct {
	*syncpkg.LocalFS
	// reads/wholeReads 用 atomic（2026-10-23）：staging 后台下载 goroutine 与测试主
	// goroutine 并发读写 ⇒ 普通 int 会被 -race 抓（实测失败）。
	reads      atomic.Int64
	wholeReads atomic.Int64
}

func (f *cacheProbeFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	f.reads.Add(1)
	return f.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

// OpenRead 记录整文件读次数（整块 staging 下载路径经此入口）。
func (f *cacheProbeFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	f.wholeReads.Add(1)
	return f.LocalFS.OpenRead(ctx, path)
}

// newCacheEnv 构造**大块**（块 > 窗口）的 secretdata FS：块 4MiB 定长、blocklet 256KiB，
// 窗口场景 = 顺序读 1MiB（4 个窗口 256KiB）落在同一块内 ⇒ 缓存后底层请求应 = 1。
func newCacheEnv(t *testing.T, cacheBudget sizex.ByteSize) (*SecretdataFS, *cacheProbeFS, []byte) {
	t.Helper()
	inner := &cacheProbeFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(inner, Options{
		Secret:             []byte("test-secret-key-000"),
		Algorithm:          testAlgo,
		BlockletCacheBytes: cacheBudget,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 2 << 20, Max: 2 << 20,
			BlockletMode: "fixed", BlockletMin: 256 << 10, BlockletMax: 256 << 10,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(4 << 20) // 4MiB（恰一块）
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, inner, content
}

// TestBlockletCache_MultiWindowReadsChunkOnce：同一块的 4 个 256KiB 窗口 ⇒ 底层 Range
// 请求只发 **1 次**（缓存后其余窗口按明文偏移切）。这是「窗口读 vs 网络读」解耦的核心断言。
func TestBlockletCache_MultiWindowReadsChunkOnce(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, 0) // 默认预算
	before := inner.reads.Load()

	var got []byte
	for off := 0; off < 1<<20; off += 256 << 10 {
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", int64(off), 256<<10)
		if err != nil {
			t.Fatalf("OpenRangeRead(%d): %v", off, err)
		}
		seg, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll(%d): %v", off, rerr)
		}
		got = append(got, seg...)
	}
	if !bytes.Equal(got, content[:1<<20]) {
		t.Fatalf("多窗口拼出内容与源不一致：len=%d", len(got))
	}
	if (inner.reads.Load() - before) != 2 {
		t.Fatalf("同一块 4 窗口底层 Range 请求次数=%d，应为 2（salt 32B 1 次 + 数据跨度 1 次；其余窗口缓存命中）", (inner.reads.Load() - before))
	}
}

// TestBlockletCache_DisabledIsZeroRegression：显式负预算（关闭缓存）⇒ 每窗口仍各自请求
// （零回归，与修复前一致）。
func TestBlockletCache_DisabledIsZeroRegression(t *testing.T) {
	t.Parallel()
	fs, inner, content := newCacheEnv(t, -1) // 关闭缓存
	before := inner.reads.Load()

	var got []byte
	for off := 0; off < 1<<20; off += 256 << 10 {
		rc, err := fs.OpenRangeRead(context.Background(), "big.bin", int64(off), 256<<10)
		if err != nil {
			t.Fatalf("OpenRangeRead(%d): %v", off, err)
		}
		seg, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll(%d): %v", off, rerr)
		}
		got = append(got, seg...)
	}
	if !bytes.Equal(got, content[:1<<20]) {
		t.Fatalf("关闭缓存后内容仍应正确：len=%d", len(got))
	}
	if (inner.reads.Load() - before) != 8 {
		t.Fatalf("关闭缓存每窗口应各自请求：底层 Range 次数=%d，应为 8（4 窗口 × 2：salt+数据）", (inner.reads.Load() - before))
	}
}

// TestBlockletCache_OverwriteInvalidates：覆盖写同一文件后，旧块的缓存明文不得被后续读
// 命中（防陈旧明文）。断言：覆盖后再次读 ⇒ 底层请求次数增加（缓存已失效）。
func TestBlockletCache_OverwriteInvalidates(t *testing.T) {
	t.Parallel()
	fs, inner, _ := newCacheEnv(t, 0)

	// 先读触发缓存回填（**必须读尽**：OpenRangeRead 返回惰性流，未读不发请求）。
	before := inner.reads.Load()
	rc0, oerr := fs.OpenRangeRead(context.Background(), "big.bin", 0, 256<<10)
	if oerr != nil {
		t.Fatalf("首次读: %v", oerr)
	}
	if _, rerr := io.ReadAll(rc0); rerr != nil {
		t.Fatalf("首次读尽: %v", rerr)
	}
	_ = rc0.Close()
	afterFirst := inner.reads.Load()
	if afterFirst == before {
		t.Fatal("首次读应发生底层请求（缓存回填）")
	}

	// 覆盖写新内容。
	newContent := data(1 << 20)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(newContent), int64(len(newContent)), 0); werr != nil {
		t.Fatalf("覆盖写: %v", werr)
	}
	rc, err := fs.OpenRangeRead(context.Background(), "big.bin", 0, int64(len(newContent)))
	if err != nil {
		t.Fatalf("覆盖后读: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("覆盖后 ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, newContent) {
		t.Fatalf("覆盖后内容应为新内容：len=%d", len(got))
	}
	if inner.reads.Load() == afterFirst {
		t.Fatal("覆盖写后缓存应失效（旧明文被命中）⇒ 底层请求次数不应不变")
	}
}
