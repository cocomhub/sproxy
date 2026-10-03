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

// TestDelete_ImmediatePhysicalCleanup（方案 A 守护：删即释放）：Delete **即时物理删**
// meta + 全部分块（无墓碑、无 GC 依赖）→ 磁盘无残留；重启 loadIndex 不重建该文件、
// 分块亦不存在。
func TestDelete_ImmediatePhysicalCleanup(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["a.bin"]
	container, chunkName := e.dirSeg, e.meta.Chunks[0].FileName
	// 记录删除前该容器全部非目录文件（meta + 分块），用于断言删后无残留。
	before := map[string]struct{}{}
	inner, _ := fs.inner.ListDir(ctx, container)
	for _, f := range inner {
		if !f.IsDir {
			before[f.Name] = struct{}{}
		}
	}
	if len(before) < 2 {
		t.Fatalf("删除前容器应含 meta+分块（至少 2 个非目录文件），got %d", len(before))
	}
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ent, _ := fs.Stat(ctx, "a.bin"); ent != nil {
		t.Error("删除后 Stat 应 nil")
	}
	// 即时物理删：删除前出现的非目录文件（meta + 分块）不再残留磁盘（无墓碑保留）；
	// 目录 meta（@ 标记）恒保留（容器结构锚点），不计残留。
	// 容器可能因成空触发 pruneEmptyDirs 整体回收（ListDir 报错 = 无残留，同样通过）。
	after, aerr := fs.inner.ListDir(ctx, container)
	if aerr == nil {
		for _, f := range after {
			if f.IsDir || shardseal.ClassifyName(f.Name) == shardseal.KindDirMeta {
				continue
			}
			if _, was := before[f.Name]; was {
				t.Errorf("Delete 后磁盘仍残留删除前文件 %s（应即时删，无墓碑）", f.Name)
			}
		}
	}
	// 重启：不重建该文件，分块亦不在。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if ent, _ := fs2.Stat(ctx, "a.bin"); ent != nil {
		t.Error("即时删后重启不应重建该文件（loadIndex 无墓碑可查）")
	}
	if ent, _ := fs2.inner.Stat(ctx, path.Join(container, chunkName)); ent != nil {
		t.Error("删除后分块应物理删除（无墓碑保留）")
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
	for i := range n {
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
	for i := range n {
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
	for i := range n {
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

// faultMetaReadFS 包装底层 FS：对指定路径的 OpenRead 返回错误（模拟挂载期瞬时读故障，
// 如云盘/IO 抖动）。failN 次内失败，之后恢复——用于验证「挂载跳过 → GC 磁盘重确认存活
// → 不误删」的守护语义（Imp-3 复审回归）。
type faultMetaReadFS struct {
	mu   sync.Mutex
	wrap syncpkg.FS
	path string // 故障路径（文件 meta blob 全路径）
	fail int    // 剩余失败次数（瞬时故障窗口）
}

func (f *faultMetaReadFS) failPathOnce(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 && p == f.path {
		f.fail--
		return true
	}
	return false
}

func (f *faultMetaReadFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.wrap.ListDir(ctx, p)
}
func (f *faultMetaReadFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *faultMetaReadFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if f.failPathOnce(p) {
		return nil, fmt.Errorf("faultMetaReadFS: 模拟瞬时读故障 %s", p)
	}
	return f.wrap.OpenRead(ctx, p)
}
func (f *faultMetaReadFS) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) error {
	return f.wrap.WriteFile(ctx, p, r, size, mtime)
}
func (f *faultMetaReadFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *faultMetaReadFS) Delete(ctx context.Context, p string) error  { return f.wrap.Delete(ctx, p) }
func (f *faultMetaReadFS) MakeDir(ctx context.Context, p string) error { return f.wrap.MakeDir(ctx, p) }

// TestGC_MountMetaReadFault_PreservesLiveFile（Imp-3 复审回归，final-review-2 第 2 轮）：
// 挂载期某容器文件 meta 读失败（瞬时故障）→ loadIndex 静默跳过该文件（不在内存索引）→
// GC 必须磁盘重确认**不删除**该存活文件（磁盘 meta + 分块仍在、可读回）；而正常孤儿分块
// （无任何 meta 引用）仍被 GC 清理。
func TestGC_MountMetaReadFault_PreservesLiveFile(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	realInner := syncpkg.NewLocalFS(root, nil)
	// 阶段 1：正常写入存活文件 f1.bin + 记录其 meta blob 路径与分块路径。
	fs1, err := NewFS(realInner, Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	writeContent(t, fs1, ctx, "f1.bin", 300)
	e1 := fs1.index["f1.bin"]
	metaPath := path.Join(e1.dirSeg, e1.metaName)
	chunkPaths := make([]string, 0, len(e1.meta.Chunks))
	for _, ci := range e1.meta.Chunks {
		chunkPaths = append(chunkPaths, path.Join(e1.dirSeg, ci.FileName))
	}
	// 阶段 2：向同容器注入一个孤儿分块（无任何 meta 引用，应被 GC 清理）。
	orphanChunk := path.Join(e1.dirSeg, "000000000000000000000000000000000000")
	if werr := realInner.WriteFile(ctx, orphanChunk, bytes.NewReader(data(64)), 64, 0); werr != nil {
		t.Fatalf("写孤儿分块: %v", werr)
	}
	// 阶段 3：模拟挂载瞬时故障——f1.bin 的 meta blob 在 loadIndex 期间读失败（跳过）。
	// fail=1：仅 loadIndex 那次读失败；之后（GC 重确认）读正常 → 判定存活。
	faulty := &faultMetaReadFS{wrap: realInner, path: metaPath, fail: 1}
	fs2, err := NewFS(faulty, Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS(faulty): %v", err)
	}
	if _, ok := fs2.index["f1.bin"]; ok {
		t.Fatal("挂载瞬时故障应使 f1.bin 不在内存索引（loadIndex 跳过）")
	}
	// 阶段 4：GC —— 重确认 f1.bin 存活 → 不误删；孤儿分块清理。
	if _, gerr := fs2.GC(ctx); gerr != nil {
		t.Fatalf("GC: %v", gerr)
	}
	// f1.bin 磁盘 meta blob 仍在（未误删）——LocalFS.Stat 对不存在返回 (nil,nil)。
	if ent, _ := realInner.Stat(ctx, metaPath); ent == nil {
		t.Error("GC 误删存活文件 f1.bin 的 meta（挂载瞬时故障 ≠ 孤儿）")
	}
	// f1.bin 磁盘分块仍在。
	for _, cp := range chunkPaths {
		if ent, _ := realInner.Stat(ctx, cp); ent == nil {
			t.Errorf("GC 误删存活文件 f1.bin 的分块 %s", path.Base(cp))
		}
	}
	// 孤儿分块被清理（无 meta 引用，判定孤儿删）。
	if ent, _ := realInner.Stat(ctx, orphanChunk); ent != nil {
		t.Error("孤儿分块（无 meta 引用）应被 GC 清理")
	}
	// 重新挂载（无故障）→ f1.bin 可读回（数据未丢）。
	fs3, err := NewFS(realInner, Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS(clean): %v", err)
	}
	rc, rerr := fs3.OpenRead(ctx, "f1.bin")
	if rerr != nil {
		t.Fatalf("重新挂载后 f1.bin 应可读（数据未丢）: %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(300)) {
		t.Errorf("f1.bin 内容不一致（GC 误删/数据丢失）")
	}
}

// TestGC_MountMetaReadFault_UnconfirmedSkipsChunkSweep（Imp-3 复审补充）：挂载瞬时故障在
// GC 时仍未恢复（meta 读持续失败）→ 无法确认存活 → fail-closed：GC 不得删该 meta，且
// **本轮不清任何分块**（未确认 meta 可能引用它们，误删即数据丢失）。
func TestGC_MountMetaReadFault_UnconfirmedSkipsChunkSweep(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	realInner := syncpkg.NewLocalFS(root, nil)
	fs1, err := NewFS(realInner, Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	writeContent(t, fs1, ctx, "f1.bin", 300)
	e1 := fs1.index["f1.bin"]
	metaPath := path.Join(e1.dirSeg, e1.metaName)
	chunkPaths := make([]string, 0, len(e1.meta.Chunks))
	for _, ci := range e1.meta.Chunks {
		chunkPaths = append(chunkPaths, path.Join(e1.dirSeg, ci.FileName))
	}
	orphanChunk := path.Join(e1.dirSeg, "111111111111111111111111111111111111")
	if werr := realInner.WriteFile(ctx, orphanChunk, bytes.NewReader(data(64)), 64, 0); werr != nil {
		t.Fatalf("写孤儿分块: %v", werr)
	}
	// 故障持续（fail 很大）：loadIndex 跳过 + GC 重确认也失败 → 未确认 → 全不清。
	faulty := &faultMetaReadFS{wrap: realInner, path: metaPath, fail: 1000}
	fs2, err := NewFS(faulty, Options{
		Secret: []byte("test-secret-key-000"), Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS(faulty): %v", err)
	}
	if _, ok := fs2.index["f1.bin"]; ok {
		t.Fatal("挂载故障应使 f1.bin 不在内存索引")
	}
	if _, gerr := fs2.GC(ctx); gerr != nil {
		t.Fatalf("GC: %v", gerr)
	}
	// fail-closed：f1.bin meta + 分块均不被误删。
	if ent, _ := realInner.Stat(ctx, metaPath); ent == nil {
		t.Error("GC 误删未确认存活的 f1.bin meta")
	}
	for _, cp := range chunkPaths {
		if ent, _ := realInner.Stat(ctx, cp); ent == nil {
			t.Errorf("GC 误删未确认存活的 f1.bin 分块 %s", path.Base(cp))
		}
	}
	// 孤儿分块本轮不清（存在未确认 meta → 全局停扫，fail-closed 优先于清扫）。
	if ent, _ := realInner.Stat(ctx, orphanChunk); ent == nil {
		t.Error("存在未确认 meta 时本轮应停扫全部孤儿分块（fail-closed），孤儿却被误删")
	}
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
