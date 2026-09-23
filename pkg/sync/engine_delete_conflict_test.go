// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// engine_delete_conflict_test.go 验证删除传播冲突语义（roadmap P2 无删除传播残余：
// 双向连续同步下删除传播的冲突语义）：
//  1. 删除前目标 mtime 变更（枚举后被改）→ 保留目标 + ActionSkippedConflict。
//  2. 删除前目标已不存在 → 幂等成功（ActionDeleted）。

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestEngineSync_DeletePropagate_TargetModifiedSinceEnum 删除前目标被修改 → 保留。
//
// 用可变 stat 桩模拟「枚举后 mtime 变化」：WalkEntries 枚举返回 Dst entry 的 mtime
// 为 t0；删除前 dst.Stat 返回 t1（>t0）→ 引擎判定冲突保留。
func TestEngineSync_DeletePropagate_TargetModifiedSinceEnum(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "keep.txt", "keep")
	writeTestFile(t, dstRoot, "keep.txt", "keep")
	writeTestFile(t, dstRoot, "gone.txt", "gone")

	engine := &Engine{Concurrency: 1}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip, DeletePolicy: DeletePropagate}
	// 覆盖 dst.Stat：对 gone.txt 返回比枚举时更新的 mtime（模拟同步期间被修改）。
	dstFS := &mtimeBumpFS{LocalFS: NewLocalFS(dstRoot, nil), bumpPath: "gone.txt"}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), dstFS, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if !localExists(t, dstRoot, "gone.txt") {
		t.Fatalf("目标同步期间被修改 → 应保留 gone.txt")
	}
	// 记一条 skipped_conflict。
	found := false
	for _, r := range job.Results {
		if r.Path == "gone.txt" && r.Action == ActionSkippedConflict {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录 skipped_conflict 结果")
	}
	if job.Stats.FilesDeleted != 0 {
		t.Fatalf("FilesDeleted 应为 0（冲突保留），got %d", job.Stats.FilesDeleted)
	}
}

// mtimeBumpFS 包装 LocalFS：对 bumpPath 的 Stat 返回未来 mtime（模拟期间被改）。
// 嵌入 *LocalFS 使其余 FS 方法（Delete/Walk 等）透传。
type mtimeBumpFS struct {
	*LocalFS
	bumpPath string
}

func (m *mtimeBumpFS) Stat(ctx context.Context, path string) (*Entry, error) {
	e, err := m.LocalFS.Stat(ctx, path)
	if err != nil {
		return nil, err
	}
	if path == m.bumpPath {
		e.MTime += int64(time.Hour)
	}
	return e, nil
}

// TestEngineSync_DeletePropagate_TargetGoneSinceEnum 删除前目标已不存在 → 幂等成功。
func TestEngineSync_DeletePropagate_TargetGoneSinceEnum(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, srcRoot, "keep.txt", "keep")
	writeTestFile(t, dstRoot, "keep.txt", "keep")
	writeTestFile(t, dstRoot, "gone.txt", "gone")

	engine := &Engine{Concurrency: 1}
	dstFS := &goneSinceEnumFS{LocalFS: NewLocalFS(dstRoot, nil), gonePath: "gone.txt"}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictSkip, DeletePolicy: DeletePropagate}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), dstFS, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// 引擎视角：Stat 已 NotExist → 记 deleted（幂等删除完成）。
	found := false
	for _, r := range job.Results {
		if r.Path == "gone.txt" && r.Action == ActionDeleted {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录 deleted（幂等）结果，got %+v", job.Results)
	}
	if job.Stats.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted 应为 1，got %d", job.Stats.FilesDeleted)
	}
}

// goneSinceEnumFS 包装 LocalFS：仅对 gone.txt 的 Stat 返回 os.ErrNotExist
// （模拟该文件在枚举后被并发删除），其余透传。
type goneSinceEnumFS struct {
	*LocalFS
	gonePath string
}

func (g *goneSinceEnumFS) Stat(ctx context.Context, path string) (*Entry, error) {
	if path == g.gonePath {
		return nil, os.ErrNotExist
	}
	return g.LocalFS.Stat(ctx, path)
}
