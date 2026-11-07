// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// capacity_accounting_test.go —— **CapacityFS 记账语义**的全面回归（用户要求「各种卷
// 在不同操作后配额都正确」）。分支重构曾删去 736 行旧记账测试，此处补回等价覆盖：
//
//   - WriteFile：新建记账、覆盖变大（净增）、覆盖变小（释放）、超限拒绝（不改变 used）；
//   - WriteIfAbsent：目标存在 no-op、不存在记账；
//   - Delete：释放；Rename/Move：源已在账、目标旧字节释放；
//   - Copy：源 TryAdd + 目标旧字节释放。

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// mapFS 是最小内存 sync.FS（记账测试用；Stat 对不存在返回 os.ErrNotExist）。
type mapFS struct {
	mu    sync.Mutex
	files map[string]string
}

func (f *mapFS) read(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.files[p]
	return s, ok
}

func (f *mapFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, _, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = string(b)
	return nil
}

func (f *mapFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	s, ok := f.read(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	return &syncpkg.Entry{Name: p, Path: p, Size: int64(len(s))}, nil
}

func (f *mapFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

func (f *mapFS) Rename(_ context.Context, from, to string) error {
	s, ok := f.read(from)
	if !ok {
		return os.ErrNotExist
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[to] = s
	delete(f.files, from)
	return nil
}

func (f *mapFS) Move(_ context.Context, from, to string) error {
	return f.Rename(context.Background(), from, to)
}

func (f *mapFS) Copy(_ context.Context, from, to string) error {
	s, ok := f.read(from)
	if !ok {
		return os.ErrNotExist
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[to] = s
	return nil
}

func (f *mapFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	s, ok := f.read(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(s)), nil
}

func (f *mapFS) WriteIfAbsent(_ context.Context, p string, r io.Reader, _, _ int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.files[p]; ok {
		return false, nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	f.files[p] = string(b)
	return true, nil
}

func (f *mapFS) MakeDir(_ context.Context, _ string) error                    { return nil }
func (f *mapFS) ListDir(_ context.Context, _ string) ([]syncpkg.Entry, error) { return nil, nil }

var _ syncpkg.FS = (*mapFS)(nil)

// newAccountTestFS 构造带容量上限的 CapacityFS（真实 LedgerCounter 记账）。
func newAccountTestFS(t *testing.T, max int64) (*CapacityFS, *LedgerCounter) {
	t.Helper()
	led, err := OpenLedger(t.TempDir(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLedgerCounter(led, max)
	return Wrap(&mapFS{files: map[string]string{}}, c), c
}

func writeN(t *testing.T, fs syncpkg.FS, p string, n int64) error {
	t.Helper()
	return fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, p, strings.NewReader(strings.Repeat("x", int(n))), n, 0)
}

// TestCapacityFS_WriteFileAccountsAndOverwrite 新建记账 + 覆盖变大（净增）+
// 覆盖变小（释放）：配额恒等于磁盘实际占用。
func TestCapacityFS_WriteFileAccountsAndOverwrite(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatalf("新建写 100: %v", err)
	}
	if c.Used() != 100 {
		t.Fatalf("新建后 Used=%d, want 100", c.Used())
	}
	// 覆盖变大：净增 100 → 200。
	if err := writeN(t, fs, "a.bin", 200); err != nil {
		t.Fatalf("覆盖写 200: %v", err)
	}
	if c.Used() != 200 {
		t.Fatalf("覆盖变大后 Used=%d, want 200（净增记账）", c.Used())
	}
	// 覆盖变小：释放 150 → 50。
	if err := writeN(t, fs, "a.bin", 50); err != nil {
		t.Fatalf("覆盖写 50: %v", err)
	}
	if c.Used() != 50 {
		t.Fatalf("覆盖变小后 Used=%d, want 50（净释放）", c.Used())
	}
}

// TestCapacityFS_OverrunRejectedKeepsUsed 超限拒绝（fail-closed）：写失败不改变 used。
func TestCapacityFS_OverrunRejectedKeepsUsed(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 100)

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatalf("首次写 100: %v", err)
	}
	if err := writeN(t, fs, "b.bin", 1); err == nil {
		t.Fatal("超限（100+1>100）应被拒绝")
	}
	if c.Used() != 100 {
		t.Fatalf("超限拒绝后 Used=%d, want 100（不改变）", c.Used())
	}
}

// TestCapacityFS_DeleteReleases 删除释放：配额归零。
func TestCapacityFS_DeleteReleases(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)
	ctx := context.Background()

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if c.Used() != 0 {
		t.Fatalf("删除后 Used=%d, want 0", c.Used())
	}
}

// TestCapacityFS_RenameReleasesOverwrittenTarget Rename 目标被覆盖：目标旧字节释放、
// 源已在账（总占用 = 源大小）。
func TestCapacityFS_RenameReleasesOverwrittenTarget(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)
	ctx := context.Background()

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatal(err)
	}
	if err := writeN(t, fs, "b.bin", 30); err != nil {
		t.Fatal(err)
	}
	// a(100) 已在账；Rename a→b 覆盖 b(30)：目标旧 30 释放，总占用 = 100。
	if err := fs.Rename(ctx, "a.bin", "b.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if c.Used() != 100 {
		t.Fatalf("Rename 覆盖后 Used=%d, want 100", c.Used())
	}
}

// TestCapacityFS_MoveSameSemanticsAsRename Move 与 Rename 同记账语义。
func TestCapacityFS_MoveSameSemanticsAsRename(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)
	ctx := context.Background()

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatal(err)
	}
	if err := writeN(t, fs, "b.bin", 30); err != nil {
		t.Fatal(err)
	}
	if err := fs.Move(ctx, "a.bin", "b.bin"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if c.Used() != 100 {
		t.Fatalf("Move 覆盖后 Used=%d, want 100", c.Used())
	}
}

// TestCapacityFS_CopyReservesSourceAndReleasesTarget Copy：源 TryAdd（总占用翻倍）+
// 目标旧字节释放。
func TestCapacityFS_CopyReservesSourceAndReleasesTarget(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)
	ctx := context.Background()

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatal(err)
	}
	if err := writeN(t, fs, "b.bin", 30); err != nil {
		t.Fatal(err)
	}
	// Copy a(100)→b：目标 b(30) 被覆盖释放，总占用 = 100(源) + 100(新 b) = 200。
	if err := fs.Copy(ctx, "a.bin", "b.bin"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if c.Used() != 200 {
		t.Fatalf("Copy 后 Used=%d, want 200", c.Used())
	}
}

// TestCapacityFS_WriteIfAbsent 目标存在 no-op（不记账）、不存在记账。
func TestCapacityFS_WriteIfAbsent(t *testing.T) {
	t.Parallel()
	fs, c := newAccountTestFS(t, 1000)
	ctx := context.Background()

	if err := writeN(t, fs, "a.bin", 100); err != nil {
		t.Fatal(err)
	}
	// 目标已存在：no-op，不得重复记账。
	done, err := fs.WriteIfAbsent(ctx, "a.bin", strings.NewReader(strings.Repeat("y", 50)), 50, 0)
	if err != nil || done {
		t.Fatalf("WriteIfAbsent 已存在: done=%v err=%v, want false/nil", done, err)
	}
	if c.Used() != 100 {
		t.Fatalf("WriteIfAbsent 已存在后 Used=%d, want 100（不得重复记账）", c.Used())
	}
	// 目标不存在：记账。
	done, err = fs.WriteIfAbsent(ctx, "c.bin", strings.NewReader(strings.Repeat("z", 80)), 80, 0)
	if err != nil || !done {
		t.Fatalf("WriteIfAbsent 新建: done=%v err=%v, want true/nil", done, err)
	}
	if c.Used() != 180 {
		t.Fatalf("WriteIfAbsent 新建后 Used=%d, want 180", c.Used())
	}
}
