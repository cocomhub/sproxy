// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// hybrid_journal_degraded_test.go 补 #1347 的**持续性**盲点。
//
// #1347 让「分片完成」只追加 WAL、不再重写 manifest（内存为主、文件仅恢复用），并加了
// 「WAL 写失败 ⇒ manifestFallback 写一次完整 manifest + journalDegraded」的兜底。
//
// 但那个兜底是**一次性**的：回落写完一次 manifest 后，后续分片仍会继续尝试 WAL；
// 若 WAL **持续**失败，则每个分片都走 `manifestFallback`（每次都全量重写 manifest）——
// 功能正确（进度不丢），但回到了 #1342 要消除的 O(n²) 写放大，且日志会刷屏。
//
// 本用例锁定两个可观测事实（不猜实现细节）：
//  1. **持续失败时进度不丢**（后完成的分片也必须出现在 manifest 里）；
//  2. **写放大有界**：不应与分片数线性增长地全量重写——理想是回落一次后进入"降级模式"
//     按节流写；至少要求「写次数 < 分片数」，否则等于没降级。
//
// 断的是"行为下限"，不锁死具体次数：将来若改成"降级后按节流写"，写次数只会更少，仍通过。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

// TestMarkChunkDone_PersistentJournalFailureKeepsProgressBoundedWrites 持续 WAL 故障下
// 进度不丢 + manifest 全量重写次数受控。
func TestMarkChunkDone_PersistentJournalFailureKeepsProgressBoundedWrites(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "degraded.bin")
	hd := &HybridDownloader{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fj := &fakeJournal{chunkErr: errors.New("wal down persistently")}
	const chunkSize = 2 << 20
	const chunks = 8
	total := int64(chunkSize * chunks)
	dc := &downloadCtx{
		currentTotal: total, chunkSize: chunkSize, destPath: dest,
		shareID: "EXAMPLE-DEGRADED", target: &ShareFile{ID: "f10", Size: total, Hash: "H10"},
		journal: fj,
	}
	src := manifestSource{ShareID: "EXAMPLE-DEGRADED", FileID: "f10", Size: total, Hash: "H10"}
	for i := 0; i < chunks; i++ {
		hd.markChunkDone(context.Background(), dc, chunk{offset: int64(i) * chunkSize, length: chunkSize}, src)
	}

	// ① 降级标记必须置位（可观测）。
	if !dc.journalDegraded {
		t.Fatal("持续 WAL 故障后应处于降级状态（journalDegraded）")
	}
	// ② 进度不丢：降级后**内存镜像**必须完整（这是进程内的单一事实源；
	//    文件按节流写，落盘时点内的最新几块可能还没写——这正是节流的语义，不是丢进度）。
	if dc.manifest == nil || len(dc.manifest.Chunks) != chunks {
		t.Fatalf("降级后内存镜像应含全部 %d 个分片，got %+v", chunks, dc.manifest)
	}
	// ②b 且至少有落盘记录（崩溃恢复依据存在）。
	m := loadManifest(dest)
	if m == nil || len(m.Chunks) == 0 {
		t.Fatalf("降级后必须至少写过一次 manifest（否则崩溃恢复无依据）: %+v", m)
	}
	// ③ 写放大受控：全量重写次数必须 < 分片数（否则等于每块都整文件重写 = O(n²) 回归）。
	if dc.manifestWrites >= chunks {
		t.Fatalf("持续 WAL 故障下 manifest 全量重写 %d 次（分片数 %d）⇒ 退化为 O(n²) 每块重写；"+
			"应进入降级模式（回落一次后按节流写）", dc.manifestWrites, chunks)
	}
}
