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

// faultWriteFS 包装底层 FS：对 WriteFile 触发 fail 次故障（模拟底层写失败，墓碑/写路径
// 注入用）。fail 递减至 0 后恢复正常（重试成功）。match 非 nil 时仅匹配路径（否则任意）。
type faultWriteFS struct {
	mu    sync.Mutex
	wrap  syncpkg.FS
	fail  int
	match func(p string) bool
}

func (f *faultWriteFS) failPath(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail <= 0 {
		return false
	}
	if f.match != nil && !f.match(p) {
		return false
	}
	f.fail--
	return true
}

func (f *faultWriteFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.wrap.ListDir(ctx, p)
}
func (f *faultWriteFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *faultWriteFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.wrap.OpenRead(ctx, p)
}
func (f *faultWriteFS) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) error {
	if f.failPath(p) {
		return fmt.Errorf("faultWriteFS: 模拟写失败 %s", p)
	}
	return f.wrap.WriteFile(ctx, p, r, size, mtime)
}
func (f *faultWriteFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *faultWriteFS) Delete(ctx context.Context, p string) error  { return f.wrap.Delete(ctx, p) }
func (f *faultWriteFS) MakeDir(ctx context.Context, p string) error { return f.wrap.MakeDir(ctx, p) }

// newFSSharedInner 构造 secretdata FS（可复用底层 inner 重建 = 模拟重启挂载）。
func newFSSharedInner(t *testing.T, inner syncpkg.FS) *SecretdataFS {
	t.Helper()
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// TestOptimisticLock_BaseVersionPersistsAcrossReload（Imp-1 守护）：写路径在加密 meta 前
// 设 Meta.BaseVersion=nextVer 落盘 → 重启 loadIndex 恢复 volVersion=max(磁盘 BaseVersion)
// 非 0；跨重启用旧 expected 写 → ErrVersionConflict（旧版本不匹配新版本），用当前版本才
// 成功——「跨进程/跨重启 CAS」从磁盘版本真正成立。
func TestOptimisticLock_BaseVersionPersistsAcrossReload(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	ctx := context.Background()

	// 阶段 1：写入，记下写前版本与落盘 meta.BaseVersion。
	fs1 := newFSSharedInner(t, inner)
	cur := fs1.CurrentVersion() // 0
	if err := fs1.WriteFile(ctx, "a.bin", bytes.NewReader(data(100)), 100, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := fs1.CurrentVersion(); got != cur+1 {
		t.Fatalf("写后卷版本=%d，want %d", got, cur+1)
	}
	e1 := fs1.index["a.bin"]
	if e1.baseVersion != cur+1 {
		t.Fatalf("内存 baseVersion=%d，want %d", e1.baseVersion, cur+1)
	}
	// 落盘 meta 必须携带版本（Imp-1 修复核心：encryptMetaBlob 前设 BaseVersion）。
	if e1.meta.BaseVersion != cur+1 {
		t.Fatalf("落盘 meta.BaseVersion=%d，want %d（乐观锁版本未写入 meta）", e1.meta.BaseVersion, cur+1)
	}

	// 阶段 2：重启（复用同一底层 inner → 重新 loadIndex）。
	fs2 := newFSSharedInner(t, inner)
	if got := fs2.CurrentVersion(); got != cur+1 {
		t.Fatalf("重启后卷版本=%d，want %d（磁盘 BaseVersion 未恢复）", got, cur+1)
	}
	e2 := fs2.index["a.bin"]
	if e2.baseVersion != cur+1 || e2.meta.BaseVersion != cur+1 {
		t.Fatalf("重启后条目版本不一致：baseVersion=%d meta.BaseVersion=%d", e2.baseVersion, e2.meta.BaseVersion)
	}

	// 阶段 3：跨重启用旧 expected（重启前版本）写 → 冲突。
	err := fs2.WriteFileIfVersion(ctx, "b.bin", bytes.NewReader(data(50)), 50, 0, cur)
	if !errors.Is(err, ErrVersionConflict) {
		t.Errorf("跨重启用旧 expected=%d 应冲突，got %v", cur, err)
	}
	if ent, _ := fs2.Stat(ctx, "b.bin"); ent != nil {
		t.Error("冲突写入不应产生条目")
	}
	// 用重启后当前版本写 → 成功，版本继续推进。
	if err := fs2.WriteFileIfVersion(ctx, "b.bin", bytes.NewReader(data(50)), 50, 0, fs2.CurrentVersion()); err != nil {
		t.Fatalf("当前版本写入应成功: %v", err)
	}
	if got := fs2.CurrentVersion(); got != cur+2 {
		t.Errorf("再次写入后卷版本=%d，want %d", got, cur+2)
	}
}

// faultDeleteFS 包装底层 FS：对 Delete 触发 fail 次故障（模拟底层物理删失败）。fail 递减
// 至 0 后恢复正常。match 非 nil 时仅匹配路径（否则任意）。
type faultDeleteFS struct {
	mu    sync.Mutex
	wrap  syncpkg.FS
	fail  int
	match func(p string) bool
}

func (f *faultDeleteFS) failPath(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail <= 0 {
		return false
	}
	if f.match != nil && !f.match(p) {
		return false
	}
	f.fail--
	return true
}

func (f *faultDeleteFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.wrap.ListDir(ctx, p)
}
func (f *faultDeleteFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *faultDeleteFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.wrap.OpenRead(ctx, p)
}
func (f *faultDeleteFS) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) error {
	return f.wrap.WriteFile(ctx, p, r, size, mtime)
}
func (f *faultDeleteFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *faultDeleteFS) Delete(ctx context.Context, p string) error {
	if f.failPath(p) {
		return fmt.Errorf("faultDeleteFS: 模拟物理删失败 %s", p)
	}
	return f.wrap.Delete(ctx, p)
}
func (f *faultDeleteFS) MakeDir(ctx context.Context, p string) error { return f.wrap.MakeDir(ctx, p) }

// TestDelete_BestEffortPhysical_NoLedgerDrift（Imp-4 语义在新方案下的守护）：Delete 不再写
// 墓碑（方案 A 即时物理删），物理删为 best-effort——即使单个分块物理删除失败，Delete 仍
// 成功且**内存账本/索引一次性原子提交**（usage 只扣一次、版本只推进一次、条目移出索引、
// 文件不可读）；幂等重试不重复扣减（无「先写墓碑失败 → 账本漂移」路径）。
func TestDelete_BestEffortPhysical_NoLedgerDrift(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := &faultDeleteFS{wrap: syncpkg.NewLocalFS(root, nil)}
	ctx := context.Background()
	fs := newFSSharedInner(t, inner)

	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	usage0 := fs.Usage()
	ver0 := fs.CurrentVersion()
	if usage0 != 300 {
		t.Fatalf("写后 usage=%d，want 300", usage0)
	}

	// 注入一次物理删除失败（best-effort）：Delete 仍成功、账本一次性扣减、无漂移。
	inner.mu.Lock()
	inner.fail = 1
	inner.mu.Unlock()
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("best-effort 物理删不应因单个分块删除失败而报错: %v", err)
	}
	if got := fs.Usage(); got != usage0-300 {
		t.Errorf("Delete 后 usage=%d，want %d（账本漂移/未一次性扣减）", got, usage0-300)
	}
	if got := fs.CurrentVersion(); got != ver0+1 {
		t.Errorf("Delete 后卷版本=%d，want %d（版本推进不符）", got, ver0+1)
	}
	if _, ok := fs.index["a.bin"]; ok {
		t.Fatal("Delete 后文件应已移出索引")
	}
	if rc, rerr := fs.OpenRead(ctx, "a.bin"); rerr == nil {
		rc.Close()
		t.Error("删除后文件应不可读")
	}

	// 幂等重试：文件已不在索引，Delete 无操作、账本不再扣减。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("幂等 Delete 应成功: %v", err)
	}
	if got := fs.Usage(); got != usage0-300 {
		t.Errorf("幂等 Delete 后 usage=%d，want %d（重复扣减）", got, usage0-300)
	}
	if got := fs.CurrentVersion(); got != ver0+1 {
		t.Errorf("幂等 Delete 后卷版本=%d，want %d（版本重复推进）", got, ver0+1)
	}
}

// TestOverwrite_Failure_RollsBackNewKeepsOld（方案 A 目标 2 守护）：覆盖写中途失败
// （rollbackWrite 删新留旧）→ **旧数据完好可读** + **无新版本残留**（新 meta + 已上传分块
// 被回滚删除）、索引仍指向旧条目、usage/版本不变。
//
// 注入点选在 **meta 上传**（chunk 已全部上传后的最后一个 WriteFile）：faultWriteFS 仅对
// 文件 meta blob 路径（含 -/_ 标记）失败——全部分块先落盘 → rollbackWrite 必须把已上传的
// 新分块 + 新 meta 一并回滚删除（否则即为孤儿残留）。
func TestOverwrite_Failure_RollsBackNewKeepsOld(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := &faultWriteFS{
		wrap: syncpkg.NewLocalFS(root, nil),
		// 仅对文件 meta blob（含 -/_ 标记）路径失败——覆盖写的 meta 上传点。
		match: func(p string) bool {
			return shardseal.ClassifyName(path.Base(p)) == shardseal.KindFileMeta
		},
	}
	fs := newFSSharedInner(t, inner)
	ctx := context.Background()

	// v1 写入。
	if err := fs.WriteFile(ctx, "ov.bin", bytes.NewReader(data(800)), 800, 0); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	oldEntry := fs.index["ov.bin"]
	oldMeta := oldEntry.metaName
	usage0 := fs.Usage()
	ver0 := fs.CurrentVersion()

	// 覆盖写：meta 上传注入失败一次 → WriteFile 应返回错误。
	inner.mu.Lock()
	inner.fail = 1
	inner.mu.Unlock()
	if err := fs.WriteFile(ctx, "ov.bin", bytes.NewReader(data(1000)), 1000, 0); err == nil {
		t.Fatal("覆盖写中途 meta 上传失败应返回错误")
	}

	// 旧数据完好：索引仍指向旧条目、meta 名不变、可读回 v1 内容。
	cur := fs.index["ov.bin"]
	if cur == nil || cur.metaName != oldMeta {
		t.Error("覆盖写失败后索引应仍指向旧版本条目（删新留旧）")
	}
	rc, rerr := fs.OpenRead(ctx, "ov.bin")
	if rerr != nil {
		t.Fatalf("覆盖写失败后 OpenRead: %v", rerr)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(buf, data(800)) {
		t.Error("覆盖写失败后应读到旧版本内容（v1 完好）")
	}
	// usage / 版本不变（回滚不推进账本、不产生新条目）。
	if fs.Usage() != usage0 {
		t.Errorf("覆盖写失败后 usage=%d，want %d（回滚推进账本）", fs.Usage(), usage0)
	}
	if fs.CurrentVersion() != ver0 {
		t.Errorf("覆盖写失败后版本=%d，want %d（回滚推进版本）", fs.CurrentVersion(), ver0)
	}
	// 无新残留：容器内除旧条目 meta+分块外，无新增 v2 分块 / meta（rollbackWrite 已删已上传）。
	// 目录 meta（@ 标记）恒保留，不计残留。
	oldChunks := map[string]struct{}{}
	for _, ci := range oldEntry.meta.Chunks {
		oldChunks[ci.FileName] = struct{}{}
	}
	entries, _ := fs.inner.ListDir(ctx, oldEntry.dirSeg)
	var extra []string
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) == shardseal.KindDirMeta {
			continue // 目录条目 / 目录 meta 恒保留
		}
		if f.Name == oldMeta {
			continue
		}
		if kind := shardseal.ClassifyName(f.Name); kind == shardseal.KindChunk {
			if _, ok := oldChunks[f.Name]; ok {
				continue // 旧版本分块
			}
		}
		extra = append(extra, f.Name)
	}
	if len(extra) > 0 {
		t.Errorf("覆盖写失败后容器残留新版本文件（未回滚删除）: %v", extra)
	}
}

// TestDelete_MetaDeleteFailure_PreservesChunksNoGhost（修复轮 Imp-1 守护：删除顺序先删 meta
// 再删分块）。mock 底层仅使 **meta 删除失败**（faultDeleteFS 只对 KindFileMeta 路径失败）
// → 分块**不被误删**，重启 loadIndex 重建**完整文件**（meta 声明 size 且分块俱在、可读回），
// 无幽灵条目。反向顺序（先删分块后删 meta）下本测试必失败（meta 残留 + 分块缺 → 幽灵）。
func TestDelete_MetaDeleteFailure_PreservesChunksNoGhost(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := &faultDeleteFS{
		wrap: syncpkg.NewLocalFS(root, nil),
		// 仅使文件 meta blob（含 -/_ 标记）删除失败——验证「meta 删失败时分块保留」。
		match: func(p string) bool {
			return shardseal.ClassifyName(path.Base(p)) == shardseal.KindFileMeta
		},
	}
	fs := newFSSharedInner(t, inner)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(300)), 300, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := fs.index["a.bin"]
	chunkName := e.meta.Chunks[0].FileName

	// 注入 meta 删除失败：Delete 仍成功（best-effort），但磁盘 meta+分块**俱在**（无幽灵面）。
	inner.mu.Lock()
	inner.fail = 1
	inner.mu.Unlock()
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete best-effort 物理删不应因 meta 删失败而报错: %v", err)
	}
	if _, ok := fs.index["a.bin"]; ok {
		t.Fatal("内存索引应已删除该条目（账本不回滚）")
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(e.dirSeg, chunkName)); ent == nil {
		t.Fatal("meta 删失败时**分块不应被删**（先删 meta 顺序）；分块若已删 → 幽灵面")
	}
	// 重启（同一底层）→ meta + 分块俱在 → 文件重建为**完整文件**（非幽灵：可读回、内容一致）。
	fs2 := newFSSharedInner(t, inner)
	if fs2.index["a.bin"] == nil {
		t.Fatal("meta 删失败且分块保留 → 重启应重建完整文件（删除丢失，非幽灵）")
	}
	rc, rerr := fs2.OpenRead(ctx, "a.bin")
	if rerr != nil {
		t.Fatalf("重启重建文件应可读（分块未删）: %v", rerr)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(buf, data(300)) {
		t.Errorf("重启重建文件内容应与 v1 一致（分块未被误删，不得为幽灵）")
	}
}
