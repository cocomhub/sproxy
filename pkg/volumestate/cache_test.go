// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fakeFS 是最小 sync.FS（记录调用次数，供「是否回源」断言）。
type fakeFS struct {
	mu       sync.Mutex
	dirs     map[string][]syncpkg.Entry
	files    map[string]string
	listCall int
	gen      uint64
	writeErr error
}

func newFakeFS() *fakeFS {
	return &fakeFS{dirs: map[string][]syncpkg.Entry{}, files: map[string]string{}}
}

func (f *fakeFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	return append([]syncpkg.Entry(nil), f.dirs[normalizePath(p)]...), nil
}

func (f *fakeFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.files[normalizePath(p)]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Path: normalizePath(p), Size: int64(len(v))}, nil
}

func (f *fakeFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.files[normalizePath(p)]
	if !ok {
		return nil, syncpkg.ErrUnsupported
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func (f *fakeFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, _ int64, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	b, _ := io.ReadAll(r)
	f.files[normalizePath(p)] = string(b)
	return nil
}

func (f *fakeFS) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[normalizePath(to)] = f.files[normalizePath(from)]
	delete(f.files, normalizePath(from))
	return nil
}

func (f *fakeFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, normalizePath(p))
	return nil
}

func (f *fakeFS) MakeDir(_ context.Context, _ string) error { return nil }

// CopyFile 模拟「同卷对象级复制」可选能力（secretdata blob 直拷的形态）。
func (f *fakeFS) CopyFile(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[normalizePath(to)] = f.files[normalizePath(from)]
	return nil
}

// StateGen 实现 volume.StateGeneration（同包测试用）。
func (f *fakeFS) StateGen(_ context.Context, _ string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen, nil
}

func (f *fakeFS) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCall
}

// newTestCache 构造仅内存缓存（Now 可控）。
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func newTestCache(t *testing.T, ttl time.Duration) (*Cache, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Unix(1000, 0)}
	return New(Options{
		TTL: func(string) time.Duration { return ttl },
		Now: clk.now,
	}), clk
}

// TestCache_HitSecondCall 钉住（读加速的基础）：同目录二次 ListDir 不回源。
func TestCache_HitSecondCall(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	fs.dirs["alice/user/docs"] = []syncpkg.Entry{{Path: "alice/user/docs/a.txt", Size: 3}}
	c, _ := newTestCache(t, time.Minute)
	wrapped := Wrap("v1", fs, c)

	for range 3 {
		if _, err := wrapped.ListDir(context.Background(), "alice/user/docs"); err != nil {
			t.Fatalf("ListDir: %v", err)
		}
	}
	if got := fs.calls(); got != 1 {
		t.Fatalf("三次 list 应只回源 1 次，got %d（缓存未命中）", got)
	}
}

// TestCache_TTLExpiry 钉住 TTL：过期后回源一次。
func TestCache_TTLExpiry(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	c, clk := newTestCache(t, 30*time.Second)
	wrapped := Wrap("v1", fs, c)

	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	clk.t = clk.t.Add(31 * time.Second)
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if got := fs.calls(); got != 2 {
		t.Fatalf("TTL 过期后应回源 2 次，got %d", got)
	}
}

// TestCache_DefaultTTLNeverExpires 钉住默认策略：**永不过期**（用户裁定 2026-10-29）。
//
// 语义：缓存是正常路径，本进程写由事件写穿；**进程外**改动（运维直写盘/同根多实例）不会
// 被自动感知——必须显式 `POST /api/volumes/refresh`（或卷自报代际变化）。
// 变异：把 DefaultTTL 改回非 0（如 5 分钟）⇒ 本用例红（时钟推进后回源）。
func TestCache_DefaultTTLNeverExpires(t *testing.T) {
	t.Parallel()
	if DefaultTTL != 0 {
		t.Fatalf("默认 TTL 必须为 0（永不过期），got %v", DefaultTTL)
	}
	fs := newFakeFS()
	clk := &testClock{t: time.Unix(1000, 0)}
	c := New(Options{Now: clk.now}) // 不注入 TTL ⇒ 走默认策略
	wrapped := Wrap("v1", fs, c)

	for _, step := range []time.Duration{0, time.Hour, 30 * 24 * time.Hour, 10 * 365 * 24 * time.Hour} {
		clk.t = clk.t.Add(step)
		if _, err := wrapped.ListDir(context.Background(), "alice/user"); err != nil {
			t.Fatalf("ListDir: %v", err)
		}
	}
	if got := fs.calls(); got != 1 {
		t.Fatalf("默认永不过期：十年级时间跨度内应只回源 1 次，got %d", got)
	}
}

// TestCache_GenerationChange 钉住代际：卷自报代际变化即回源（跨进程收敛路径）。
func TestCache_GenerationChange(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	fs.gen = 7
	c, _ := newTestCache(t, 0) // TTL=0：不按时间过期，只看代际
	wrapped := Wrap("v1", fs, c)

	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if got := fs.calls(); got != 1 {
		t.Fatalf("代际未变应命中，got %d 次回源", got)
	}
	fs.gen = 8
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if got := fs.calls(); got != 2 {
		t.Fatalf("代际变化应回源，got %d 次回源", got)
	}
}

// TestCache_InvalidateMakesStale 钉住失效：写后 list 立即反映（P3 机制）。
func TestCache_InvalidateMakesStale(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	fs.dirs["alice/user/docs"] = []syncpkg.Entry{{Path: "alice/user/docs/a.txt"}}
	c, _ := newTestCache(t, 0)
	wrapped := Wrap("v1", fs, c)

	_, _ = wrapped.ListDir(context.Background(), "alice/user/docs")
	// 经装饰器写入（无需显式登记失效）⇒ 下一次 list 必须回源。
	if err := wrapped.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/docs/b.txt",
		strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.dirs["alice/user/docs"] = []syncpkg.Entry{
		{Path: "alice/user/docs/a.txt"}, {Path: "alice/user/docs/b.txt"},
	}
	got, err := wrapped.ListDir(context.Background(), "alice/user/docs")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("写入后 list 应反映新条目，got %d 条（缓存未失效）", len(got))
	}
	if n := fs.calls(); n != 2 {
		t.Fatalf("写后应回源（第 2 次），got %d", n)
	}
}

// TestCache_InvalidateBubblesToAncestors 钉住祖先 stale：写入子目录后**父目录**视图失效。
func TestCache_InvalidateBubblesToAncestors(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	fs.dirs["alice/user"] = []syncpkg.Entry{{Path: "alice/user/docs", IsDir: true}}
	c, _ := newTestCache(t, 0)
	wrapped := Wrap("v1", fs, c)

	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	c.Invalidate(ObjectRef{Volume: "v1", Path: "alice/user/docs/new.txt"})
	fs.dirs["alice/user"] = []syncpkg.Entry{
		{Path: "alice/user/docs", IsDir: true},
		{Path: "alice/user/new.txt"},
	}
	got, _ := wrapped.ListDir(context.Background(), "alice/user")
	if len(got) != 2 {
		t.Fatalf("子目录新增后父目录 list 应回源并见 2 条，got %d（祖先未标记 stale）", len(got))
	}
}

// TestCache_WriteErrorKeepsCache 钉住：写失败**不**失效（避免无谓回源）。
func TestCache_WriteErrorKeepsCache(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	fs.writeErr = errors.New("boom")
	c, _ := newTestCache(t, 0)
	wrapped := Wrap("v1", fs, c)

	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	_ = wrapped.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/x.txt",
		strings.NewReader("x"), 1, 0)
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if n := fs.calls(); n != 1 {
		t.Fatalf("写失败不应失效缓存，got %d 次回源", n)
	}
}

// TestCache_DisabledTTLAlwaysFetch 钉住「TTL<0 = 禁用缓存」。
func TestCache_DisabledTTLAlwaysFetch(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	c, _ := newTestCache(t, -1)
	wrapped := Wrap("v1", fs, c)
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if n := fs.calls(); n != 2 {
		t.Fatalf("禁用缓存应每次回源，got %d", n)
	}
}

// TestCache_LRUEviction 钉住容量上限（防无界内存）。
func TestCache_LRUEviction(t *testing.T) {
	t.Parallel()
	clk := &testClock{t: time.Unix(1000, 0)}
	c := New(Options{TTL: func(string) time.Duration { return 0 }, MaxEntries: 2, Now: clk.now})
	fs := newFakeFS()
	wrapped := Wrap("v1", fs, c)
	for _, p := range []string{"alice/user/a", "alice/user/b", "alice/user/c"} {
		_, _ = wrapped.ListDir(context.Background(), p)
	}
	if n := c.Len(); n > 2 {
		t.Fatalf("LRU 上限 2，got %d", n)
	}
}

// TestStore_PersistAcrossCaches 钉住落盘：新 Cache 实例（模拟重启）可复用快照。
func TestStore_PersistAcrossCaches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := NewStore(root, 1<<20, nil)
	if !st.Enabled() {
		t.Fatal("应启用落盘")
	}
	fs := newFakeFS()
	c1 := New(Options{TTL: func(string) time.Duration { return time.Hour }, Store: st})
	_, _ = Wrap("v1", fs, c1).ListDir(context.Background(), "alice/user/docs")

	// 新实例（空内存 LRU）应命中磁盘快照 ⇒ 不回源。
	c2 := New(Options{TTL: func(string) time.Duration { return time.Hour }, Store: NewStore(root, 1<<20, nil)})
	_, _ = Wrap("v1", fs, c2).ListDir(context.Background(), "alice/user/docs")
	if n := fs.calls(); n != 1 {
		t.Fatalf("重启后应从磁盘快照命中，got %d 次回源", n)
	}
}

// TestStore_VolumeMismatchDiscarded 钉住归属校验：串卷快照一律丢弃。
func TestStore_VolumeMismatchDiscarded(t *testing.T) {
	t.Parallel()
	st := NewStore(t.TempDir(), 1<<20, nil)
	st.Save("volA", "alice/user", &DirSnapshot{Volume: "volB", Entries: []syncpkg.Entry{{Path: "x"}}})
	if _, ok := st.Load("volA", "alice/user"); ok {
		t.Fatal("串卷快照必须丢弃（fail-closed 到回源）")
	}
}

// TestCache_NormalizeAndAncestors 钉住路径归一与祖先链（含卷根）。
func TestCache_NormalizeAndAncestors(t *testing.T) {
	t.Parallel()
	if got := normalizePath("/a//b/"); got != "a/b" {
		t.Fatalf("normalizePath = %q, want a/b", got)
	}
	got := ancestorPaths("a/b/c")
	want := []string{"a/b", "a", ""}
	if len(got) != len(want) {
		t.Fatalf("ancestors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ancestors = %v, want %v", got, want)
		}
	}
}

// TestWrap_CopyFileInvalidatesTarget 钉住「同卷对象复制」快路径也必须失效目标：
// 该能力由卷实现、经 `Innermost` 探测；若装饰器不转发（或调用方直连最内层），
// 复制结果会直到 TTL 才出现在列表里（默认 5 分钟，不可接受）。
func TestWrap_CopyFileInvalidatesTarget(t *testing.T) {
	t.Parallel()
	fs := newFakeFS()
	c, _ := newTestCache(t, time.Minute)
	wrapped := Wrap("v1", fs, c)

	if _, err := wrapped.ListDir(context.Background(), "owner/user/d"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if n := fs.calls(); n != 1 {
		t.Fatalf("首次 list 应回源 1 次，got %d", n)
	}
	cp, ok := wrapped.(interface {
		CopyFile(ctx context.Context, from, to string) error
	})
	if !ok {
		t.Fatal("CacheFS 应转发 CopyFile（否则调用方只能直连最内层，绕过失效）")
	}
	if err := cp.CopyFile(context.Background(), "owner/user/d/a.bin", "owner/user/d/b.bin"); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if _, err := wrapped.ListDir(context.Background(), "owner/user/d"); err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if n := fs.calls(); n != 2 {
		t.Fatalf("复制后应失效并回源（2 次），got %d（失效未发生）", n)
	}
}
