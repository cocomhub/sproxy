// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// TestEngineSync_Merge3_NoConflict 引擎接入：源改一行 + 目标旧版本保留 →
// conflict_policy=merge3 自动合并（无冲突标记），目标 = 源内容。
func TestEngineSync_Merge3_NoConflict(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, dstRoot, "a.txt", "a\nb\nc\n")
	// 旧目标 mtime 设早（src 新文件 mtime 较晚）→ size 同但 mtime 不同 → diff 判 updated。
	setTestMTime(t, dstRoot, "a.txt", 1000000)
	writeTestFile(t, srcRoot, "a.txt", "a\nB\nc\n")

	eng := &Engine{Concurrency: 2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	job := &Job{
		Direction:      DirectionPush,
		Src:            "", // 空 = FS 根（对齐 engine_test 模式）
		Dst:            "", // 空 = FS 根（对齐 engine_test 模式）
		ConflictPolicy: ConflictMerge3,
	}
	if err := eng.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := readLocal(t, dstRoot, "a.txt")
	if got != "a\nB\nc\n" {
		t.Fatalf("merge3 合并结果 = %q, want 源内容（无冲突自动合并）", got)
	}
	if len(job.Results) != 1 || job.Results[0].Action != ActionUpdated {
		t.Fatalf("Results = %+v, want 1 条 ActionUpdated", job.Results)
	}
}
