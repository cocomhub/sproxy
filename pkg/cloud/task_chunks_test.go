// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// TestRecordChunkState_OverwriteAndCap 锁定分块状态记录语义（用户 2026-11-26）：
// 同 Index 覆盖（pending→downloading→done 只保留最新）、超上限丢弃、负数忽略。
func TestRecordChunkState_OverwriteAndCap(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	recordChunkState(task, downloader.ChunkState{Index: 0, Phase: "pending", Length: 32 << 20})
	recordChunkState(task, downloader.ChunkState{Index: 0, Phase: "done", Done: 32 << 20, Length: 32 << 20})
	if len(task.Chunks) != 1 {
		t.Fatalf("同 Index 应覆盖，实际 %d 条", len(task.Chunks))
	}
	if task.Chunks[0].Phase != "done" || task.Chunks[0].Done != 32<<20 {
		t.Fatalf("应保留最新状态，实际 %+v", task.Chunks[0])
	}
	for i := 1; i <= chunkStateCap+5; i++ {
		recordChunkState(task, downloader.ChunkState{Index: i, Phase: "pending"})
	}
	if len(task.Chunks) != chunkStateCap {
		t.Fatalf("超上限应丢弃新块，实际 %d 条（上限 %d）", len(task.Chunks), chunkStateCap)
	}
	recordChunkState(task, downloader.ChunkState{Index: -1})
	if len(task.Chunks) != chunkStateCap {
		t.Fatal("负 Index 不应写入")
	}
}

// TestChunkProgressContext 锁定 ctx 通道（跨下载器按任务隔离的进度上报）。
func TestChunkProgressContext(t *testing.T) {
	t.Parallel()
	if downloader.ChunkProgressFrom(t.Context()) != nil {
		t.Fatal("未设置时应为 nil")
	}
	var got []downloader.ChunkState
	ctx := downloader.WithChunkProgress(t.Context(), func(st downloader.ChunkState) {
		got = append(got, st)
	})
	fn := downloader.ChunkProgressFrom(ctx)
	if fn == nil {
		t.Fatal("设置后应可取回")
	}
	fn(downloader.ChunkState{Index: 3, Phase: "downloading", Done: 1 << 20})
	if len(got) != 1 || got[0].Index != 3 {
		t.Fatalf("回调未生效: %+v", got)
	}
}
