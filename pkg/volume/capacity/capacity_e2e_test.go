// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// capacity_e2e_test.go：CapacityFS 与 pkg/sync 引擎的端到端——push 到限额外部卷超限拒绝。

import (
	"context"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memFS 是内存 sync.FS（e2e 用；写入到 map）。
type memFS struct {
	files map[string]int64 // path → size
}

func newMemFS() *memFS { return &memFS{files: map[string]int64{}} }

func (m *memFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *memFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	sz, ok := m.files[p]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: p, Path: p, Size: sz}, nil
}
func (m *memFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(strings.Repeat("x", int(m.files[p])))), nil
}
func (m *memFS) WriteFile(_ context.Context, p string, _ io.Reader, size int64, _ int64) error {
	m.files[p] = size
	return nil
}
func (m *memFS) Rename(context.Context, string, string) error { return nil }
func (m *memFS) Delete(_ context.Context, p string) error {
	delete(m.files, p)
	return nil
}
func (m *memFS) MakeDir(context.Context, string) error { return nil }

// TestCapacityFS_SyncEngine_ExceedsRejected 钉住 sync 引擎 push 到限额满的外部卷：
// WriteFile 超限返回错误 → 引擎文件失败（不写网盘）。
func TestCapacityFS_SyncEngine_ExceedsRejected(t *testing.T) {
	t.Parallel()
	inner := newMemFS()
	c := NewCounter(50, "") // 限额 50 字节
	fs := Wrap(inner, c)

	// 先写入 30 字节（占 60% 限额）。
	if err := fs.WriteFile(context.Background(), "a.txt", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(a.txt 30): %v", err)
	}
	// sync 引擎 push b.txt（40 字节 → 超限 70>50）。
	engine := &syncpkg.Engine{Concurrency: 1}
	job := &syncpkg.Job{
		Direction:      syncpkg.DirectionPush,
		Src:            "",
		Dst:            "",
		Recursive:      true,
		ConflictPolicy: syncpkg.ConflictSkip,
	}
	local := syncpkg.NewLocalFS(t.TempDir(), nil)
	// 本地写入 b.txt 40 字节。
	if err := local.WriteFile(context.Background(), "b.txt", strings.NewReader(strings.Repeat("y", 40)), 40, 0); err != nil {
		t.Fatalf("local.WriteFile: %v", err)
	}
	if err := engine.Sync(context.Background(), local, fs, job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 引擎 b.txt 失败（超限）——检查引擎结果。
	if _, ok := inner.files["b.txt"]; ok {
		t.Fatal("b.txt 不应写入（超限拒绝）")
	}
	if job.Stats.FilesDone != 0 {
		t.Fatalf("FilesDone = %d, want 0（b.txt 超限拒绝）", job.Stats.FilesDone)
	}
}
