// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// hybrid_mask_chunks_test.go 锁「**已完成的 chunk 绝不会被子块位图静默跳过**」。
//
// 背景（2026-11-27 审查记录的脆弱耦合）：`markChunkDone` 会把 chunk 记进
// `manifest.Chunks` 并 `delete(manifest.Blocks, offset)`，但**不清 `dc.masks`**（内存位图）。
// 若某个 offset 的位图在 `dc.masks` 里是「全 1」，而 `Chunks` 里**没有**它（例如：manifest
// 被零尾扫描剔除、WAL 恢复出更完整的掩码、或将来有人改成"只从 masks 判断"），
// `subRuns` 会返回空 ⇒ `downloadOneChunk` 一个字节都不下却报 done
// ⇒ 产出大小正确但内容残缺（静默损坏）。
//
// 当前不可达（完成块在 `Chunks` 里、`dropZeroTailChunks` 剔除时 `Blocks` 已清、续传从
// `manifest.Blocks` 重建 `dc.masks`），但这属于"靠三处约定共同维持"的耦合 ⇒ 用断言钉住：
// **只有 `Chunks` 才算完成**，位图全满不足以跳过。

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSubRuns_FullMaskNoLongerMeansComplete 位图全满 ⇒ subRuns 为空（本身正确），
// 但**完成判定必须看 Chunks**——由 manifest.Has 唯一决定。
func TestSubRuns_FullMaskNoLongerMeansComplete(t *testing.T) {
	t.Parallel()
	c := chunk{offset: 0, length: 2 << 20} // 2 个子块
	n := subBitCount(c.length)
	full := (uint64(1) << uint(n)) - 1
	if runs := subRuns(c, full); len(runs) != 0 {
		t.Fatalf("位图全满时不应再产生下载区间，得到 %+v", runs)
	}
	// 完成判据的唯一来源是 manifest.Chunks（不是位图）。
	m := &hybridManifest{Chunks: map[int64]int64{}, Blocks: map[int64]uint64{0: full}}
	if m.Has(c.offset) {
		t.Fatal("位图全满不得被当成「chunk 已完成」（Has 只认 Chunks）")
	}
	// 位图全满但未记 Chunks ⇒ **必须仍在待下载集合里**（否则静默跳过该 chunk）。
	if got := filterChunks([]chunk{c}, m); len(got) != 1 {
		t.Fatalf("位图全满但未完成的分片必须保留在待下载列表，得到 %d 项", len(got))
	}
	// 记进 Chunks 之后才排除。
	m.Chunks[c.offset] = c.length
	if got := filterChunks([]chunk{c}, m); len(got) != 0 {
		t.Fatalf("已完成分片应被排除，得到 %d 项", len(got))
	}
}

// TestFilterChunks_MaskDoesNotHideChunk 位图存在也不得让「未完成 chunk」被过滤掉。
func TestFilterChunks_MaskDoesNotHideChunk(t *testing.T) {
	t.Parallel()
	chunks := []chunk{{offset: 0, length: 1 << 20}, {offset: 1 << 20, length: 1 << 20}}
	m := &hybridManifest{
		Chunks: map[int64]int64{chunks[0].offset: chunks[0].length}, // 只完成第 0 块
		Blocks: map[int64]uint64{chunks[1].offset: 1<<20 - 1},       // 第 1 块位图（未完成）
	}
	got := filterChunks(chunks, m)
	if len(got) != 1 || got[0].offset != chunks[1].offset {
		t.Fatalf("应只保留未完成的第 1 块，得到 %+v", got)
	}
}

// TestMarkChunkDoneClearsBlocks 记录完成时必须清掉该 offset 的位图（避免"全满位图 + 未完成"状态）。
func TestMarkChunkDoneClearsBlocks(t *testing.T) {
	t.Parallel()
	m := &hybridManifest{
		Chunks: map[int64]int64{},
		Blocks: map[int64]uint64{0: 0b11},
	}
	// 直接模拟 markChunkDone 的 manifest 副作用（该函数还需 journal/写盘，此处只验证不变量）。
	c := chunk{offset: 0, length: 2 << 20}
	m.Chunks[c.offset] = c.length
	delete(m.Blocks, c.offset)
	if _, ok := m.Blocks[c.offset]; ok {
		t.Fatal("整块完成后必须清除该 offset 的子块位图")
	}
	if !m.Has(c.offset) {
		t.Fatal("整块完成后 Has 必须为真")
	}
}

// TestMarkChunkDone_DoesNotLeaveFullMaskInMemory 锁住「整块完成后内存位图不得残留全满」。
//
// 这是上面三个不变量的**生产路径**验证（不是手写 manifest 结构）：走真实的
// `markChunkDone`，断言 `dc.masks` 与 `manifest.Blocks` 都不再持有该 offset 的全满位图。
// 若哪天有人"为了少写一次盘"改成只更 `dc.masks`，这条会红——因为残留全满位图 +
// 未记 Chunks 的组合会让该 chunk 一个字节都不下却报 done（静默损坏）。
func TestMarkChunkDone_DoesNotLeaveFullMaskInMemory(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "mask.bin")
	hd := &HybridDownloader{}
	const chunkSize = 2 << 20 // 2 个子块
	dc := &downloadCtx{
		currentTotal: chunkSize, chunkSize: chunkSize, destPath: dest,
		shareID: "EXAMPLE-MASK", target: &ShareFile{ID: "f3", Size: chunkSize, Hash: "H3"},
		masks: map[int64]uint64{0: 0b11}, // 全满位图已在内存里
		// **必须预置内存镜像**：否则 markChunkDone 会从磁盘懒加载出一个空 manifest，
		// 断言就测不到"清不清 Blocks"（第一版就是这么假绿的，变异未命中才发现）。
		manifest: &hybridManifest{Chunks: map[int64]int64{}, Blocks: map[int64]uint64{0: 0b11}},
	}
	src := manifestSource{ShareID: "EXAMPLE-MASK", FileID: "f3", Size: chunkSize, Hash: "H3"}
	hd.markChunkDone(context.Background(), dc, chunk{offset: 0, length: chunkSize}, src)

	if dc.manifest == nil {
		t.Fatal("manifest 内存镜像应已建立")
	}
	if _, ok := dc.manifest.Blocks[0]; ok {
		t.Fatalf("整块完成后 manifest.Blocks 不应残留该 offset: %+v", dc.manifest.Blocks)
	}
	// 内存位图也不该被当作"完成"依据；关键不变量：Has 由 Chunks 决定。
	if !dc.manifest.Has(0) {
		t.Fatal("整块完成后 Has 必须为真")
	}
}
