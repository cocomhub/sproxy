// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// capacity_test.go 钉住 C2 卷级计数记账：写入累计 / 删除释放 / 超限拒绝 / 持久化。
//
// 用户定案（2026-09-18）：卷级计数记账——外部卷写入累计 + 删除释放，超限拒绝；
// 本系统可用限额（UserVolume.Capacity）强制生效。

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ---- VolumeCapacityCounter：写入累计 / 释放 / 超限拒绝 / 持久化 ----

// TestCounter_TryAdd_RespectsCapacity 钉住超限拒绝：used + size > capacity → 错误。
func TestCounter_TryAdd_RespectsCapacity(t *testing.T) {
	t.Parallel()
	c := NewCounter(100, "")
	if err := c.TryAdd(60); err != nil {
		t.Fatalf("TryAdd(60) = %v, want nil", err)
	}
	if got := c.Used(); got != 60 {
		t.Fatalf("Used = %d, want 60", got)
	}
	if err := c.TryAdd(50); err == nil {
		t.Fatal("TryAdd(50) 超限应返回错误（60+50>100）")
	}
	if got := c.Used(); got != 60 {
		t.Fatalf("TryAdd 失败后 Used = %d, want 60（不变）", got)
	}
	c.Release(60)
	if got := c.Used(); got != 0 {
		t.Fatalf("Release 后 Used = %d, want 0", got)
	}
}

// TestCounter_ZeroCapacity_Unlimited 钉住 capacity=0 语义：不限（只记账不拒绝）。
func TestCounter_ZeroCapacity_Unlimited(t *testing.T) {
	t.Parallel()
	c := NewCounter(0, "")
	if err := c.TryAdd(1 << 40); err != nil {
		t.Fatalf("capacity=0 应不限制: %v", err)
	}
}

// TestCounter_Persist_Roundtrip 钉住持久化：TryAdd 后 Save，Load 恢复 used。
func TestCounter_Persist_Roundtrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cap.json")
	c := NewCounter(100, path)
	if err := c.TryAdd(40); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c2, err := Load(path, 100)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c2.Used(); got != 40 {
		t.Fatalf("Load 后 Used = %d, want 40", got)
	}
}

// ---- CapacityFS 装饰器：WriteFile 累计 / Delete 释放 / 超限拒绝 ----

// recordingFS 是记录 WriteFile/Delete 调用的 fake sync.FS（内存 size 表，支持覆盖写）。
type recordingFS struct {
	mu     sync.Mutex
	sizes  map[string]int64
	writes atomic.Int64
	dels   atomic.Int64
}

func newRecordingFS() *recordingFS { return &recordingFS{sizes: map[string]int64{}} }

func (r *recordingFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (r *recordingFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (r *recordingFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (r *recordingFS) WriteFile(_ context.Context, p string, _ io.Reader, size, _ int64) error {
	r.writes.Add(1)
	r.mu.Lock()
	r.sizes[p] = size
	r.mu.Unlock()
	return nil
}
func (r *recordingFS) Rename(_ context.Context, from, to string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sizes[from]; ok {
		r.sizes[to] = s
		delete(r.sizes, from)
	}
	return nil
}
func (r *recordingFS) Delete(_ context.Context, p string) error {
	r.dels.Add(1)
	r.mu.Lock()
	delete(r.sizes, p)
	r.mu.Unlock()
	return nil
}
func (r *recordingFS) MakeDir(context.Context, string) error { return nil }

// TestCapacityFS_WriteAddsAndDeleteReleases 钉住装饰器：新建 WriteFile 累计 size，Delete 释放。
func TestCapacityFS_WriteAddsAndDeleteReleases(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(100, "")
	fs := Wrap(inner, c)

	if err := fs.WriteFile(context.Background(), "a.txt", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := c.Used(); got != 30 {
		t.Fatalf("WriteFile 后 Used = %d, want 30", got)
	}
	if err := fs.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.Used(); got != 0 {
		t.Fatalf("Delete 后 Used = %d, want 0", got)
	}
	if inner.writes.Load() != 1 || inner.dels.Load() != 1 {
		t.Fatalf("inner 调用: writes=%d dels=%d, want 1/1", inner.writes.Load(), inner.dels.Load())
	}
}

// TestCapacityFS_WriteExceedsCapacity_Rejected 钉住装饰器超限拒绝：
// capacity 满时 WriteFile 返回错误（不触达 inner）。
func TestCapacityFS_WriteExceedsCapacity_Rejected(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)

	if err := fs.WriteFile(context.Background(), "a.txt", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(30): %v", err)
	}
	if err := fs.WriteFile(context.Background(), "b.txt", nil, 30, 0); err == nil {
		t.Fatal("WriteFile(30) 超限（60>50）应返回错误")
	}
	if got := c.Used(); got != 30 {
		t.Fatalf("拒绝后 Used = %d, want 30（不变）", got)
	}
	if inner.writes.Load() != 1 {
		t.Fatalf("inner writes = %d, want 1（超限不触达）", inner.writes.Load())
	}
}

// TestCapacityFS_OverwriteAdjustsDelta 钉住覆盖写差分（2026-10-10）：覆盖小文件释放差额，
// 变大时只按净增收取；超净增部分被拒绝。
func TestCapacityFS_OverwriteAdjustsDelta(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "x.bin", nil, 30, 0); err != nil {
		t.Fatalf("WriteFile(30): %v", err)
	}
	if err := fs.WriteFile(ctx, "x.bin", nil, 10, 0); err != nil {
		t.Fatalf("覆盖写小文件: %v", err)
	}
	if got := c.Used(); got != 10 {
		t.Fatalf("覆盖写变小后 Used = %d, want 10", got)
	}
	// 净增 50（10 → 60）超限（10+50>50）→ 拒绝，且不触达 inner。
	writesBefore := inner.writes.Load()
	if err := fs.WriteFile(ctx, "x.bin", nil, 60, 0); err == nil {
		t.Fatal("覆盖写净增超限应拒绝")
	}
	if got := c.Used(); got != 10 {
		t.Fatalf("拒绝后 Used = %d, want 10（不变）", got)
	}
	if inner.writes.Load() != writesBefore {
		t.Fatal("超限不应触达 inner WriteFile")
	}
}

// TestCapacityFS_RenameFreesTarget 钉住改名/移动释放目标旧字节（源已在账上）。
func TestCapacityFS_RenameFreesTarget(t *testing.T) {
	t.Parallel()
	inner := newRecordingFS()
	c := NewCounter(50, "")
	fs := Wrap(inner, c)
	ctx := context.Background()

	if err := fs.WriteFile(ctx, "src.bin", nil, 20, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, "dst.bin", nil, 15, 0); err != nil {
		t.Fatal(err)
	}
	if got := c.Used(); got != 35 {
		t.Fatalf("Used = %d, want 35", got)
	}
	if err := fs.Rename(ctx, "src.bin", "dst.bin"); err != nil { // 覆盖 dst（旧 15 释放）
		t.Fatal(err)
	}
	if got := c.Used(); got != 20 {
		t.Fatalf("Rename 后 Used = %d, want 20（目标旧 15 释放）", got)
	}
}

// measuringFS 记录 WriteFile 实际读到的字节数（用于 size<=0 未知长度的实测记账断言）。
type measuringFS struct {
	mu    sync.Mutex
	sizes map[string]int64
}

func (m *measuringFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *measuringFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sizes[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, Size: s}, nil
	}
	return nil, nil
}
func (m *measuringFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (m *measuringFS) WriteFile(_ context.Context, p string, r io.Reader, size, _ int64) error {
	n := size
	if r != nil {
		w, _ := io.Copy(io.Discard, r)
		n = w
	}
	m.mu.Lock()
	m.sizes[p] = n
	m.mu.Unlock()
	return nil
}
func (m *measuringFS) Rename(context.Context, string, string) error { return nil }
func (m *measuringFS) Delete(context.Context, string) error         { return nil }
func (m *measuringFS) MakeDir(context.Context, string) error        { return nil }

// TestCapacityFS_UnknownSizeUsesMeasured 对抗评审：size<=0（未知长度，如 ContentLength=-1）
// 此前会反向记账（delta<0 → Release 抹掉他人占用）；现按实测字节入账。
func TestCapacityFS_UnknownSizeUsesMeasured(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	fs := Wrap(&measuringFS{sizes: map[string]int64{}}, NewPoolCounter(pool))
	content := strings.Repeat("x", 30)
	if err := fs.WriteFile(context.Background(), "a.bin", strings.NewReader(content), -1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := pool.Usage(); got != 30 {
		t.Fatalf("未知长度应按实测入账 30, got %d", got)
	}
	// 已知长度超限仍 fail-closed。
	if err := fs.WriteFile(context.Background(), "b.bin", nil, 80, 0); err == nil {
		t.Fatal("30+80 超限应拒绝")
	}
}
