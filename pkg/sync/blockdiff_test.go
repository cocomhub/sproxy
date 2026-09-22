// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestBlockDiff_IdenticalBlocks 相同内容 → 0 差异块。
func TestBlockDiff_IdenticalBlocks(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("x"), 3*1024*1024) // 3 MiB（3 个 1MiB 块）
	diffs, err := BlockDiff(bytes.NewReader(data), bytes.NewReader(data), 1024*1024)
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("相同文件应 0 差异块, got %v", diffs)
	}
}

// TestBlockDiff_TailAppend 尾部追加 → 只追加块差异（src 新文件 3MiB+100B → 4 块；
// dst 旧文件 3MiB → 3 块；只有第 4 块（索引 3）差异）。
func TestBlockDiff_TailAppend(t *testing.T) {
	t.Parallel()
	block := 1024 * 1024
	src := append(bytes.Repeat([]byte("a"), 3*block), bytes.Repeat([]byte("z"), 100)...) // 3MiB + 100B
	dst := bytes.Repeat([]byte("a"), 3*block)                                            // 3MiB 旧文件
	diffs, err := BlockDiff(bytes.NewReader(src), bytes.NewReader(dst), int64(block))
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 1 || diffs[0] != 3 {
		t.Fatalf("尾部追加应只差异块 3（第 4 块）, got %v", diffs)
	}
}

// TestBlockDiff_MiddleModify 中部修改 → 只中部块差异。
func TestBlockDiff_MiddleModify(t *testing.T) {
	t.Parallel()
	block := 1024 * 1024
	src := bytes.Repeat([]byte("a"), 5*block) // 5 MiB
	dst := append([]byte(nil), src...)
	// 改第 3 块（索引 2）的一个字节。
	dst[2*block+10] = 'b'
	diffs, err := BlockDiff(bytes.NewReader(src), bytes.NewReader(dst), int64(block))
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 1 || diffs[0] != 2 {
		t.Fatalf("中部修改应只差异块 2, got %v", diffs)
	}
}

// TestBlockDiff_BlockBoundary 块边界：修改正好跨两块边界 → 两块都差异。
func TestBlockDiff_BlockBoundary(t *testing.T) {
	t.Parallel()
	block := 1024 * 1024
	src := bytes.Repeat([]byte("c"), 2*block)
	dst := append([]byte(nil), src...)
	// 改边界前后各一字节（跨越块 0 与块 1 边界）。
	dst[block-1] = 'x'
	dst[block] = 'y'
	diffs, err := BlockDiff(bytes.NewReader(src), bytes.NewReader(dst), int64(block))
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 2 || diffs[0] != 0 || diffs[1] != 1 {
		t.Fatalf("跨边界修改应差异块 0 和 1, got %v", diffs)
	}
}

// TestBlockDiff_Shortened 目标比源长（旧文件有多余尾部块，源截断后）→
// src 视角：源短，前 2 块相同，无多余差异（src 只有 2 块）。
func TestBlockDiff_Shortened(t *testing.T) {
	t.Parallel()
	block := 1024 * 1024
	src := bytes.Repeat([]byte("d"), 2*block) // 新文件 2 块
	dst := bytes.Repeat([]byte("d"), 4*block) // 旧文件 4 块（源删减后）
	diffs, err := BlockDiff(bytes.NewReader(src), bytes.NewReader(dst), int64(block))
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("源短且前 2 块相同应 0 差异, got %v", diffs)
	}
}

// TestBlockDiff_EmptyDst 目标为空（无旧文件）→ 全量差异（零回归路径）。
func TestBlockDiff_EmptyDst(t *testing.T) {
	t.Parallel()
	block := 1024 * 1024
	src := bytes.Repeat([]byte("e"), 3*block)
	diffs, err := BlockDiff(bytes.NewReader(src), bytes.NewReader(nil), int64(block))
	if err != nil {
		t.Fatalf("BlockDiff: %v", err)
	}
	if len(diffs) != 3 || diffs[0] != 0 || diffs[1] != 1 || diffs[2] != 2 {
		t.Fatalf("空目标应全量差异（0,1,2）, got %v", diffs)
	}
}

// TestBlockDiff_DefaultBlockSize 默认块大小 = 1MiB。
func TestBlockDiff_DefaultBlockSize(t *testing.T) {
	t.Parallel()
	if defaultBlockSize != 1024*1024 {
		t.Fatalf("defaultBlockSize = %d, want 1MiB", defaultBlockSize)
	}
}

// TestBlockDiff_ChecksumMatch 块校验和 = SHA-256 hex（与分块上传 ChunkChecksums 同算法）。
func TestBlockDiff_ChecksumMatch(t *testing.T) {
	t.Parallel()
	blockData := []byte("hello block")
	got := blockChecksum(blockData)
	want := sha256.Sum256(blockData)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("blockChecksum = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// TestBlockDiff_LongString 辅助：大字符串重复（不直接用 bytes.Repeat 超长字面量）。
var _ = strings.Repeat
