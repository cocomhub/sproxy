// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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

// TestRecordChunkState_SnapshotSafe 锁定「块状态写时复制」：
// SnapshotTask/ListTasks 是结构体浅拷贝，HTTP 层在**解锁后**序列化快照；若更新块状态是原地写，
// 读（marshal）与写无同步 ⇒ DATA RACE。本用例在 -race 下能稳定复现旧实现的竞态。
func TestRecordChunkState_SnapshotSafe(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	recordChunkState(task, downloader.ChunkState{Index: 0, Phase: "pending", Length: 1024})
	snap := *task // 与 SnapshotTask 同语义的浅拷贝

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 2000 {
			recordChunkState(task, downloader.ChunkState{Index: 0, Phase: "downloading", Done: int64(i)})
		}
	})
	// 读快照（模拟 handler 序列化）：不得与写入共享同一底层数组。
	for range 2000 {
		if len(snap.Chunks) > 0 {
			_ = snap.Chunks[0].Phase
			_ = snap.Chunks[0].Done
		}
	}
	wg.Wait()
}

// TestSnapshot_IsolatedFromLiveMutation 锁定「快照深拷贝」：
// 快照被 HTTP 层解锁后序列化，而任务字段在锁内持续改写（Diagnostics/Chunks/Progress/Transfer）
// ⇒ 浅拷贝会让 marshal 与写入无同步（DATA RACE）。本用例在 -race 下能复现浅拷贝的竞态。
func TestSnapshot_IsolatedFromLiveMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sm := capacity.NewStorageManager(filepath.Join(root, "cap"), 16<<20, nil, testLogger())
	lfs := syncpkg.NewLocalFS(root, testLogger())
	mgr, _ := newCloudTestManager(t, root, sm, actionsTestConfig(),
		CloudManagerOptions{TransferFSFor: func(string) (syncpkg.FS, string) { return lfs, "local" }})
	task, err := mgr.CreateTask("url", "https://example.com/s/EXAMPLE-SNAP-1", "s.bin", 1024, "",
		TaskParams{Save: true, Transfer: &TransferSpec{Volume: "v", Path: "p/x.bin"}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	snap, ok := mgr.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatal("快照应存在")
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 2000 {
			mgr.mu.Lock()
			task.noteStage("downloading")
			recordChunkState(task, downloader.ChunkState{Index: 0, Phase: "downloading", Done: int64(i)})
			recordProgressSample(task, int64(i), 1024, time.Now().Add(time.Duration(i)*time.Second))
			if task.Transfer != nil {
				task.Transfer.Overwrite = i%2 == 0
			}
			mgr.mu.Unlock()
		}
	})
	// 模拟 handler 序列化快照（解锁后、无锁读）
	for range 2000 {
		_ = snap.Diagnostics
		_ = len(snap.Chunks)
		_ = len(snap.Progress)
		if snap.Transfer != nil {
			_ = snap.Transfer.Overwrite
		}
	}
	wg.Wait()
}
