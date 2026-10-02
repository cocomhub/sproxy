// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newDedupFS 建一个启用去重（Dedup=true）的 secretdata FS。
func newDedupFS(t *testing.T) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
		Dedup:   true,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// TestMTimeScatter_DefaultRandomized：默认行为——底层 blob mtime 打散（≠原始，防分片时间
// 聚类），逻辑层 entry.mtime 恒为原始；PreserveMTime=true 时透传原始 mtime。
func TestMTimeScatter_DefaultRandomized(t *testing.T) {
	t.Parallel()

	// 默认打散：底层 blob mtime 与原始不同（至少一个），逻辑层 mtime == 原始。
	fs := newFS(t)
	ctx := context.Background()
	const orig = int64(1_800_000_000_000_000_000)
	if err := fs.WriteFile(ctx, "s.bin", bytes.NewReader(data(300)), 300, orig); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["s.bin"]
	if e.mtime != orig {
		t.Errorf("逻辑层 mtime=%d，want %d（不应被打散）", e.mtime, orig)
	}
	inner, _ := fs.inner.ListDir(ctx, e.dirSeg)
	scattered := false
	for _, f := range inner {
		if f.MTime != orig {
			scattered = true
		}
	}
	if !scattered {
		t.Error("默认 mtime 应打散（至少一个底层 blob mtime ≠ 原始）")
	}
	// 重启后逻辑层 mtime 仍为 meta 原始值（不受打散/物理 blob mtime 影响）。
	fsR, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS reload: %v", err)
	}
	if fsR.index["s.bin"] == nil {
		t.Error("重启后 s.bin 应重建")
	} else if fsR.index["s.bin"].mtime != orig {
		t.Errorf("重启后逻辑层 mtime=%d，want %d", fsR.index["s.bin"].mtime, orig)
	}

	// PreserveMTime=true：底层透传原始 mtime，逻辑层仍为原始。
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fsP, perr := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(), PreserveMTime: true,
	})
	if perr != nil {
		t.Fatalf("NewFS(PreserveMTime): %v", perr)
	}
	if err := fsP.WriteFile(ctx, "p.bin", bytes.NewReader(data(300)), 300, orig); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	pe := fsP.index["p.bin"]
	if pe.mtime != orig {
		t.Errorf("PreserveMTime 逻辑层 mtime=%d，want %d", pe.mtime, orig)
	}
	innerP, _ := fsP.inner.ListDir(ctx, pe.dirSeg)
	preserved := false
	for _, f := range innerP {
		if f.MTime == orig {
			preserved = true
		}
	}
	if !preserved {
		t.Error("PreserveMTime=true 时应透传原始 mtime 到底层 blob")
	}
}

// TestDedup_SameContentSharedBlob：同内容两文件引用同一 blob、RefCount=2；删除一文件
// RefCount=1；全删归零物理删。读回内容均正确。
func TestDedup_SameContentSharedBlob(t *testing.T) {
	t.Parallel()
	fs := newDedupFS(t)
	ctx := context.Background()
	content := data(500)
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := fs.WriteFile(ctx, "b.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}
	a, b := fs.index["a.bin"], fs.index["b.bin"]
	if a == nil || b == nil {
		t.Fatal("索引缺失")
	}
	if len(a.meta.Chunks) == 0 || a.meta.Chunks[0].FileName != b.meta.Chunks[0].FileName {
		t.Error("同内容两文件应引用同一去重 blob")
	}
	key := a.meta.Chunks[0].OrigSHA256
	pool := fs.dedupPool[key]
	if pool == nil || pool.refs != 2 {
		refs := int64(-1)
		if pool != nil {
			refs = pool.refs
		}
		t.Fatalf("池引用 refs=%d，want 2", refs)
	}
	for _, name := range []string{"a.bin", "b.bin"} {
		rc, err := fs.OpenRead(ctx, name)
		if err != nil {
			t.Fatalf("OpenRead(%s): %v", name, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, content) {
			t.Errorf("%s 还原内容不一致", name)
		}
	}

	// 删除一文件 → refs=1，blob 仍存在。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete a: %v", err)
	}
	pool = fs.dedupPool[key]
	if pool == nil || pool.refs != 1 {
		refs := int64(-1)
		if pool != nil {
			refs = pool.refs
		}
		t.Fatalf("删一文件后 refs=%d，want 1", refs)
	}
	chunkName := a.meta.Chunks[0].FileName
	if ent, _ := fs.inner.Stat(ctx, path.Join(fs.dedupDir, chunkName)); ent == nil {
		t.Error("RefCount=1 时 blob 不应物理删除")
	}

	// 全删归零物理删。
	if err := fs.Delete(ctx, "b.bin"); err != nil {
		t.Fatalf("Delete b: %v", err)
	}
	if _, ok := fs.dedupPool[key]; ok {
		t.Error("全删后池条目应删除")
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(fs.dedupDir, chunkName)); ent != nil {
		t.Error("RefCount 归零后 blob 应物理删除")
	}
}

// TestOptimisticLock_VersionConflict：写前版本不一致 → 明确错误；一致 → 成功且
// BaseVersion +1、卷版本推进。
func TestOptimisticLock_VersionConflict(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()

	cur := fs.CurrentVersion()
	err := fs.WriteFileIfVersion(ctx, "v.bin", bytes.NewReader(data(100)), 100, 0, cur)
	if err != nil {
		t.Fatalf("一致版本写入应成功: %v", err)
	}
	if fs.index["v.bin"].baseVersion != cur+1 {
		t.Errorf("meta baseVersion=%d，want %d", fs.index["v.bin"].baseVersion, cur+1)
	}
	if got := fs.CurrentVersion(); got != cur+1 {
		t.Errorf("卷版本=%d，want %d", got, cur+1)
	}

	// 冲突（传入过期 cur）→ ErrVersionConflict，不产生条目。
	err2 := fs.WriteFileIfVersion(ctx, "w.bin", bytes.NewReader(data(50)), 50, 0, cur)
	if !errors.Is(err2, ErrVersionConflict) {
		t.Errorf("过期版本应报 ErrVersionConflict，got %v", err2)
	}
	if ent, _ := fs.Stat(ctx, "w.bin"); ent != nil {
		t.Error("冲突写入不应落盘")
	}

	// DeleteIfVersion：一致成功。
	if err := fs.WriteFile(ctx, "d.bin", bytes.NewReader(data(30)), 30, 0); err != nil {
		t.Fatalf("WriteFile d: %v", err)
	}
	want := fs.CurrentVersion()
	if err := fs.DeleteIfVersion(ctx, "d.bin", want); err != nil {
		t.Errorf("DeleteIfVersion 一致应成功: %v", err)
	}
	// DeleteIfVersion 冲突。
	if err := fs.WriteFile(ctx, "d2.bin", bytes.NewReader(data(40)), 40, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteIfVersion(ctx, "d2.bin", want); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("DeleteIfVersion 过期版本应冲突，got %v", err)
	}
}

// TestTombstone_SkipOnReload：Delete 写墓碑 → 重启 loadIndex 跳过该文件；GC 后才物理删
// 分块与墓碑 meta。
func TestTombstone_SkipOnReload(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["a.bin"]
	container, chunkName := e.dirSeg, e.meta.Chunks[0].FileName
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ent, _ := fs.Stat(ctx, "a.bin"); ent != nil {
		t.Error("删除后 Stat 应 nil")
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(container, chunkName)); ent == nil {
		t.Fatal("GC 前分块应保留（墓碑）")
	}

	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if ent, _ := fs2.Stat(ctx, "a.bin"); ent != nil {
		t.Error("墓碑后重启不应重建该文件（loadIndex 跳过 Deleted）")
	}

	// GC 后物理删除分块与墓碑 meta。
	if _, err := fs2.GC(ctx); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if ent, _ := fs2.inner.Stat(ctx, path.Join(container, chunkName)); ent != nil {
		t.Error("GC 后分块应物理删除")
	}
	inner, _ := fs2.inner.ListDir(ctx, container)
	for _, f := range inner {
		if shardseal.ClassifyName(f.Name) == shardseal.KindFileMeta {
			t.Errorf("GC 后墓碑 meta %q 应物理删除", f.Name)
		}
	}
}

// TestUsage_Accumulates：写/删/覆盖后 usage 正确，且重启后 loadIndex 恢复。
func TestUsage_Accumulates(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, "b.bin", bytes.NewReader(data(400)), 400, 0); err != nil {
		t.Fatal(err)
	}
	if fs.Usage() != 700 {
		t.Errorf("写后 usage=%d，want 700", fs.Usage())
	}
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatal(err)
	}
	if fs.Usage() != 400 {
		t.Errorf("删后 usage=%d，want 400", fs.Usage())
	}
	if err := fs.WriteFile(ctx, "c.bin", bytes.NewReader(data(100)), 100, 0); err != nil {
		t.Fatal(err)
	}
	if fs.Usage() != 500 {
		t.Errorf("再写后 usage=%d，want 500", fs.Usage())
	}
	// 覆盖：b 400→200。
	if err := fs.WriteFile(ctx, "b.bin", bytes.NewReader(data(200)), 200, 0); err != nil {
		t.Fatal(err)
	}
	if fs.Usage() != 300 {
		t.Errorf("覆盖后 usage=%d，want 300", fs.Usage())
	}

	// 重启恢复（a 墓碑跳过；b=200、c=100）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if fs2.Usage() != 300 {
		t.Errorf("重启后 usage=%d，want 300", fs2.Usage())
	}
}

// TestGC_PreservesLiveFiles（C-1 守护）：对一批存活文件（同一容器、互不删除）跑一次 GC，
// 全部文件仍可读回且内容一致——GC 两阶段标记不得按文件名排序把存活分块当孤儿删除。
func TestGC_PreservesLiveFiles(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	const n = 12
	contents := map[string][]byte{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%02d.bin", i)
		c := data(200 + i*13) // 各文件内容互不相同
		contents[name] = c
		if err := fs.WriteFile(ctx, name, bytes.NewReader(c), int64(len(c)), 0); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if _, err := fs.GC(ctx); err != nil {
		t.Fatalf("GC: %v", err)
	}
	for name, want := range contents {
		rc, err := fs.OpenRead(ctx, name)
		if err != nil {
			t.Fatalf("GC 后 OpenRead(%s): %v", name, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, want) {
			t.Errorf("%s 内容不一致（GC 误删存活分块）", name)
		}
	}
}

// TestOptimisticLock_ConcurrentSameVersion（I-1 守护）：两个并发带相同 expected 的
// WriteFileIfVersion 只能一胜一负（提交前再校验 CAS 原子推进版本），不能双写都成功。
// TestGC_ReusesIndex_NoMetaReread（Imp-3 回归锁）：GC 标记阶段必须复用内存索引
// s.index（挂载态与磁盘一致），不得对底层逐 meta 重读盘 + 重跑 scrypt。用计数 FS 断言：
// 写 N 个文件后跑一次 GC，期间对底层「文件 meta blob」的 OpenRead 次数必须为 0
// （index 已含全部解密后的存活 meta；只有二遍孤儿扫描走 ListDir，不回读 meta）。
func TestGC_ReusesIndex_NoMetaReread(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := &countingFS{wrap: syncpkg.NewLocalFS(root, nil)}
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	const n = 20
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%02d.bin", i)
		c := data(200 + i*7)
		if werr := fs.WriteFile(ctx, name, bytes.NewReader(c), int64(len(c)), 0); werr != nil {
			t.Fatalf("WriteFile %s: %v", name, werr)
		}
	}
	// GC 前清零计数（只统计 GC 期间 meta 读）。
	inner.resetCounts()
	if _, gerr := fs.GC(ctx); gerr != nil {
		t.Fatalf("GC: %v", gerr)
	}
	// 存活 meta blob 的 OpenRead 应为 0：Imp-3 标记走 s.index 内存镜像，不重读盘。
	// 分块扫描无需读内容（仅 ListDir）；墓碑/孤儿判定也不回读 meta。
	if metaReads := inner.metaReads(); metaReads != 0 {
		t.Errorf("GC 期间读 meta blob %d 个文件（应复用 index，0 个）；大卷会逐 meta 重跑 scrypt 阻塞", metaReads)
	}
	// GC 后文件仍全部可读（不误删）。
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%02d.bin", i)
		rc, rerr := fs.OpenRead(ctx, name)
		if rerr != nil {
			t.Fatalf("GC 后 OpenRead(%s): %v", name, rerr)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data(200+i*7)) {
			t.Errorf("%s 内容不一致（GC 误删）", name)
		}
	}
}

// countingFS 包装底层 FS，统计 GC 期间对「文件 meta blob」（文件名含 -/_ 标记、
// shardseal.ClassifyName == KindFileMeta）的 OpenRead 次数——Imp-3 断言用。
type countingFS struct {
	mu   sync.Mutex
	read map[string]int // container/name → 文件 meta blob OpenRead 次数
	wrap syncpkg.FS
}

// resetCounts 清零统计（GC 前调用）。
func (c *countingFS) resetCounts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read = map[string]int{}
}

// metaReads 返回 GC 期间读过的文件 meta blob 数（≥1 个不同 meta 名即计一次）。
func (c *countingFS) metaReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.read)
}

func (c *countingFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return c.wrap.ListDir(ctx, p)
}
func (c *countingFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return c.wrap.Stat(ctx, p)
}
func (c *countingFS) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) error {
	return c.wrap.WriteFile(ctx, p, r, size, mtime)
}
func (c *countingFS) Rename(ctx context.Context, from, to string) error {
	return c.wrap.Rename(ctx, from, to)
}
func (c *countingFS) Delete(ctx context.Context, p string) error  { return c.wrap.Delete(ctx, p) }
func (c *countingFS) MakeDir(ctx context.Context, p string) error { return c.wrap.MakeDir(ctx, p) }
func (c *countingFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	// 仅统计读文件 meta（文件名基段含 -/_ 标记 = 密文 meta）。
	if kc := shardseal.ClassifyName(path.Base(p)); kc == shardseal.KindFileMeta {
		c.mu.Lock()
		if c.read == nil {
			c.read = map[string]int{}
		}
		c.read[p]++
		c.mu.Unlock()
	}
	return c.wrap.OpenRead(ctx, p)
}

func TestOptimisticLock_ConcurrentSameVersion(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	expected := fs.CurrentVersion() // 0
	names := []string{"c1.bin", "c2.bin"}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fs.WriteFileIfVersion(ctx, names[i], bytes.NewReader(data(100+i)), int64(100+i), 0, expected)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrVersionConflict):
			t.Fatalf("意外错误（应冲突或成功）: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("并发同 expected 双写应一胜一负，成功 %d 次", ok)
	}
	// 胜者文件可读回（失败者不落盘）。
	for i, err := range errs {
		if err != nil {
			continue
		}
		rc, rerr := fs.OpenRead(ctx, names[i])
		if rerr != nil {
			t.Fatalf("胜者 OpenRead(%s): %v", names[i], rerr)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data(100+i)) {
			t.Errorf("胜者 %s 内容不一致", names[i])
		}
	}
}
