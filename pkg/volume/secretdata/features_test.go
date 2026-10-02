// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
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
