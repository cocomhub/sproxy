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

// TestReadChunkRangeBySegments_LargeBlockSplitIntoWindows：块 > maxRangeSpan（5MiB 单块）
// 时，读 256KiB 目标段 ⇒ 合并区间 = 整块 > 4MiB ⇒ 底层按窗口分 2 次 Range（各自 ≤4MiB），
// 内容精确。这是 2026-10-22 实测根因（百度 CDN 单次 Range > 4MiB 403）的锁定用例。
func TestReadChunkRangeBySegments_LargeBlockSplitIntoWindows(t *testing.T) {
	t.Parallel()
	contentSize := 5 << 20 // 5MiB
	probe := &saltSpanFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: int64(contentSize), Max: int64(contentSize),
			BlockletMode: "fixed", BlockletMin: 64 << 10, BlockletMax: 64 << 10,
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(contentSize)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "big.bin",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 读中间 256KiB（非块头）⇒ 目标段在块中后部 ⇒ 合并区间 = 整块 ⇒ 需分段。
	rc, rerr := fs.OpenRangeRead(context.Background(), "big.bin", 4<<20, 256<<10)
	if rerr != nil {
		t.Fatalf("OpenRangeRead: %v", rerr)
	}
	got, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		t.Fatalf("ReadAll: %v", readErr)
	}
	if !bytes.Equal(got, content[4<<20:(4<<20)+(256<<10)]) {
		t.Fatalf("大块分段读内容错误：len=%d", len(got))
	}
	// 5MiB 跨度 / 4MiB 窗口 ⇒ 2 段数据 + salt 32B 单独 1 次 ⇒ 3 次 Range。
	if len(probe.spans) != 3 {
		t.Fatalf("大块分段读底层 Range 请求次数=%d，应为 3（salt 32B 1 次 + 5MiB 跨度按 4MiB 窗口分 2 段）", len(probe.spans))
	}
	for _, sp := range probe.spans {
		if sp.size > maxRangeSpan {
			t.Fatalf("单次 Range 窗口 %d 超过上限 %d（百度 CDN 403 根因）", sp.size, maxRangeSpan)
		}
	}
}
