// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// segment_coalesce_test.go —— 同一块内**多个目标 blocklet 段必须合并成一次 Range 读取**
// （2026-10-21 缺陷修复：段级请求数 × 网盘 RTT 是随机读慢的主因之一）。
//
// 格式依据（pkg/cryptox/shardseal/crypto.go:642 planBlockletLayout）：同一块 blob 内相邻
// blocklet 段的密文**近似连续**（段间仅 cryptoRandN(64) = 0-63 字节随机间隙），且每段的
// 精确位置 EncOffset/EncSize 已记录在 meta ⇒ 可用**一次** `[首段.EncOffset, 末段.EncOffset+
// 末段.EncSize)` 请求覆盖全部目标段，再按已知 EncOffset 切片、逐段解密。
//
// 旧实现（`readChunkRangeBySegments`）：每段一次 `RangeReadAll` ⇒ N 段 = N 次底层 Range
// 请求（每次一记 RTT + dlink 查询）。网盘场景 N 段时延 ≈ N × RTT（实测 242KB 文件 47s）。
//
// 本文件锁定：K 个目标段 ⇒ **1 次**段读请求（+1 次 salt 读），内容与逐段一致。

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// rangeSpansFS 记录底层每次 OpenRangeRead 的区间（用于断言「合并成一次」且区间正确）。
type rangeSpansFS struct {
	*syncpkg.LocalFS
	spans []span
}

type span struct{ offset, size int64 }

func (f *rangeSpansFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	f.spans = append(f.spans, span{offset, size})
	return f.LocalFS.OpenRangeRead(ctx, path, offset, size)
}

// newCoalesceEnv 建「单块 + 多 blocklet」夹具：一个块内 8 个等长 blocklet，
// 读整块必然命中全部 8 段 —— 用于断言段读请求被合并。
func newCoalesceEnv(t *testing.T, contentSize int) (*SecretdataFS, *rangeSpansFS, []byte) {
	t.Helper()
	probe := &rangeSpansFS{LocalFS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	fs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{
			Mode: "random", Min: 512, Max: 512, // 单块（512B）
			BlockletMode: "fixed", BlockletMin: 64, BlockletMax: 64, // 8 段/块
		},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	content := data(contentSize)
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "movie.mp4",
		bytes.NewReader(content), int64(len(content)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return fs, probe, content
}

// TestReadChunkRangeBySegments_CoalescesSegments：读整块（命中全部 8 段）只发**1 次**段读
// 请求（加 1 次 salt），而不是 8 次。
func TestReadChunkRangeBySegments_CoalescesSegments(t *testing.T) {
	t.Parallel()
	fs, probe, content := newCoalesceEnv(t, 512)

	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 0, 512)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("内容不一致：len=%d", len(got))
	}
	// 期望：1 次 salt 读 + 1 次合并段读 = 2 次（旧实现：1 + 8 = 9 次）。
	if len(probe.spans) != 2 {
		offs := make([]string, 0, len(probe.spans))
		for _, s := range probe.spans {
			offs = append(offs, formatSpan(s))
		}
		t.Fatalf("底层 Range 请求次数=%d，应为 2（1 salt + 1 合并段读；旧实现每段一次 = 9 次）\n实际区间: %v",
			len(probe.spans), offs)
	}
	// 合并后的区间必须**覆盖**整块密文（≥ 512 字节明文对应的密文长）。
	if probe.spans[1].size < 512 {
		t.Fatalf("合并段读区间过小（size=%d）——未覆盖全部目标段", probe.spans[1].size)
	}
}

// TestReadChunkRangeBySegments_CoalescedPartialRange：只读块内**部分**区间（命中 2-3 段）
// 同样合并成 1 次段读，且内容精确（不多不少）。
func TestReadChunkRangeBySegments_CoalescedPartialRange(t *testing.T) {
	t.Parallel()
	fs, probe, content := newCoalesceEnv(t, 512)

	// 读 [100, 260)：覆盖第 2-5 段（64B 一段）。
	rc, err := fs.OpenRangeRead(context.Background(), "movie.mp4", 100, 160)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	got, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("ReadAll: %v", rerr)
	}
	if !bytes.Equal(got, content[100:260]) {
		t.Fatalf("部分区间内容错误：len=%d", len(got))
	}
	if len(probe.spans) != 2 {
		t.Fatalf("部分区间底层 Range 请求次数=%d，应为 2（1 salt + 1 合并段读）", len(probe.spans))
	}
}

func formatSpan(s span) string {
	return "[" + itoa(s.offset) + ",+" + itoa(s.size) + ")"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
