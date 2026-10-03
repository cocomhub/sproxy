// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// rangeAwareFS 包装 LocalFS 以断言 RangeReader 是否被使用（追踪 range 读取次数）。
type rangeAwareFS struct {
	*syncpkg.LocalFS
	rangeReads int // 非并发测试；OpenRangeRead 调用计数
}

func (r *rangeAwareFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	r.rangeReads++
	return r.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

// TestOpenRangeRead_UsesSegmentRangeReads：底层支持 RangeReader 时，随机范围读走「按
// blocklet 段局部读取」路径——range 读取次数显著少于整块数（1 次 salt + 每目标段 1 次，
// 而非整块 io.ReadAll），且明文与整读一致。
func TestOpenRangeRead_UsesSegmentRangeReads(t *testing.T) {
	t.Parallel()
	// 构造小块策略 + 小文件（4 个块），Range 读一个 blocklet 跨度的区间。
	root := t.TempDir()
	inner := &rangeAwareFS{LocalFS: syncpkg.NewLocalFS(root, nil)}
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMode: "fixed", BlockletMin: 32, BlockletMax: 64},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	content := data(512) // 跨多块
	if werr := fs.WriteFile(ctx, "movie.mp4", bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 读中间一段 [200,300)（应只涉少数 blocklet 段）。
	rc, err := fs.OpenRangeRead(ctx, "movie.mp4", 200, 100)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content[200:300]) {
		t.Fatalf("范围读内容 != 原文区间：len(got)=%d", len(got))
	}
	// range 读取必须发生（否则回退了整块路径）。
	if inner.rangeReads == 0 {
		t.Fatal("RangeReader 路径未触发（OpenRangeRead 未被调用）")
	}
	// 全量回归：OpenRead 整读一致。
	rc2, err := fs.OpenRead(ctx, "movie.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	all, err := io.ReadAll(rc2)
	rc2.Close()
	if err != nil {
		t.Fatalf("ReadAll 整读: %v", err)
	}
	if !bytes.Equal(all, content) {
		t.Fatalf("整读内容不一致")
	}
}

// TestOpenRangeRead_NonRangeBackendFallsBack：底层无 RangeReader（plain LocalFS 包装为
// 非 Range 类型）→ 回退整块读取路径，明文仍正确。
func TestOpenRangeRead_NonRangeBackendFallsBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMode: "fixed", BlockletMin: 32, BlockletMax: 64},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	content := data(600)
	if werr := fs.WriteFile(ctx, "a.mp4", bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	rc, err := fs.OpenRangeRead(ctx, "a.mp4", 250, 150)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content[250:400]) {
		t.Fatalf("非 Range 回退范围读内容 != 原文区间")
	}
}
