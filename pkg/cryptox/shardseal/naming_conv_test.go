// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"testing"
)

// TestConvNames_DeterministicClassifiable 锁定收敛命名的三点：确定性（同 blob ⇒ 同名）、
// 分类正确（ClassifyName 仍可识别 chunk/meta/dir-meta）、长度分布与 legacy 一致（35-43）。
func TestConvNames_DeterministicClassifiable(t *testing.T) {
	t.Parallel()
	blob := []byte("deterministic-blob-bytes-0123456789")
	group := "Ab3kP9zQ1"

	chunkA, err := ChunkNameConv(blob, group)
	if err != nil {
		t.Fatalf("ChunkNameConv: %v", err)
	}
	chunkB, err := ChunkNameConv(blob, group)
	if err != nil {
		t.Fatalf("ChunkNameConv(2): %v", err)
	}
	if chunkA != chunkB {
		t.Fatalf("收敛分块名必须确定性: %q vs %q", chunkA, chunkB)
	}
	if ClassifyName(chunkA) != KindChunk {
		t.Fatalf("收敛分块名应分类为 chunk: %q", chunkA)
	}
	if len(chunkA) < 35 || len(chunkA) > 43 {
		t.Fatalf("分块名长度 %d 不在 35-43", len(chunkA))
	}
	if _, serr := SplitRandSegments(chunkA); serr != nil {
		t.Fatalf("SplitRandSegments 应能解析收敛名: %v", serr)
	}

	metaName, err := MetaNameConv(blob, group)
	if err != nil {
		t.Fatalf("MetaNameConv: %v", err)
	}
	if ClassifyName(metaName) != KindFileMeta {
		t.Fatalf("收敛 file meta 名应分类为 filemeta: %q", metaName)
	}
	dirName, err := DirMetaNameConv(blob, group)
	if err != nil {
		t.Fatalf("DirMetaNameConv: %v", err)
	}
	if ClassifyName(dirName) != KindDirMeta {
		t.Fatalf("收敛 dir meta 名应分类为 dirmeta: %q", dirName)
	}
	// 三种类型名互不相同（不同域派生不同随机段）。
	if chunkA == metaName || metaName == dirName || chunkA == dirName {
		t.Fatal("不同类型收敛名应不同（域分离）")
	}

	// 不同 blob ⇒ 不同名（内容敏感）。
	other, err := ChunkNameConv([]byte("other-blob-bytes-9876543210"), group)
	if err != nil {
		t.Fatalf("ChunkNameConv(other): %v", err)
	}
	if other == chunkA {
		t.Fatal("不同 blob 应产生不同收敛名")
	}
	// 同名重复构造仍然相等（幂等）。
	if again, _ := ChunkNameConv(blob, group); again != chunkA {
		t.Fatal("收敛名应幂等")
	}
}

// TestConvNames_RandomSegCharsetExcludesMarks 验证收敛名的随机段仍剔除类型标记字符
// （否则 ClassifyName 误判）——分块名的两个随机段不得含 q/z。
func TestConvNames_RandomSegCharsetExcludesMarks(t *testing.T) {
	t.Parallel()
	for i := range 64 {
		blob := []byte{byte(i), 0xAB, 0xCD, byte(i * 7)}
		name, err := ChunkNameConv(blob, "Ab3kP9zQ1")
		if err != nil {
			t.Fatalf("ChunkNameConv: %v", err)
		}
		if ClassifyName(name) != KindChunk {
			t.Fatalf("blob#%d 收敛分块名被误分类（随机段含标记）: %q", i, name)
		}
	}
}
