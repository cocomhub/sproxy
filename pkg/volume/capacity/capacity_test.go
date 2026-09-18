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
	"sync/atomic"
	"testing"

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

// recordingFS 是记录 WriteFile/Delete 调用的 fake sync.FS（Stat 返回 a.txt 大小 30）。
type recordingFS struct {
	writes atomic.Int64
	dels   atomic.Int64
}

func (r *recordingFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (r *recordingFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	if p == "a.txt" {
		return &syncpkg.Entry{Name: "a.txt", Path: "a.txt", Size: 30}, nil
	}
	return nil, nil
}
func (r *recordingFS) OpenRead(context.Context, string) (io.ReadCloser, error) { return nil, nil }
func (r *recordingFS) WriteFile(_ context.Context, _ string, _ io.Reader, _ int64, _ int64) error {
	r.writes.Add(1)
	return nil
}
func (r *recordingFS) Rename(context.Context, string, string) error { return nil }
func (r *recordingFS) Delete(_ context.Context, _ string) error {
	r.dels.Add(1)
	return nil
}
func (r *recordingFS) MakeDir(context.Context, string) error { return nil }

// TestCapacityFS_WriteAddsAndDeleteReleases 钉住装饰器：WriteFile 累计 size，Delete 释放。
func TestCapacityFS_WriteAddsAndDeleteReleases(t *testing.T) {
	t.Parallel()
	inner := &recordingFS{}
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
	inner := &recordingFS{}
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
