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

// TestDelete_TombstoneWriteFailure_NoLedgerDrift（Imp-4 守护）：Delete 先写墓碑成功→再动
// usage/volVersion；墓碑写失败时返回错误且**不动账本**——usage/volVersion 不变、文件仍在
// 索引、可读回；重试（底层恢复）成功只扣一次、版本只推进一次。
func TestDelete_TombstoneWriteFailure_NoLedgerDrift(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := &faultWriteFS{wrap: syncpkg.NewLocalFS(root, nil)}
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

	// 墓碑写注入失败一次：Delete 应失败且不扣减。
	inner.mu.Lock()
	inner.fail = 1
	inner.mu.Unlock()
	if err := fs.Delete(ctx, "a.bin"); err == nil {
		t.Fatal("墓碑写失败时 Delete 应返回错误")
	}
	if got := fs.Usage(); got != usage0 {
		t.Errorf("墓碑失败后 usage=%d，want %d（账本被提前扣减）", got, usage0)
	}
	if got := fs.CurrentVersion(); got != ver0 {
		t.Errorf("墓碑失败后卷版本=%d，want %d（版本被提前推进）", got, ver0)
	}
	if _, ok := fs.index["a.bin"]; !ok {
		t.Fatal("墓碑失败后文件应仍在索引（不落半态）")
	}
	// 文件仍可读回（内容未损坏）。
	rc, rerr := fs.OpenRead(ctx, "a.bin")
	if rerr != nil {
		t.Fatalf("墓碑失败后 OpenRead: %v", rerr)
	}
	buf, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(buf, data(300)) {
		t.Error("墓碑失败后文件内容不一致")
	}

	// 重试（底层已恢复）：成功且只扣一次、版本只推进一次。
	if err := fs.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("重试 Delete 应成功: %v", err)
	}
	if got := fs.Usage(); got != usage0-300 {
		t.Errorf("重试后 usage=%d，want %d（双重扣减）", got, usage0-300)
	}
	if got := fs.CurrentVersion(); got != ver0+1 {
		t.Errorf("重试后卷版本=%d，want %d（版本推进不符）", got, ver0+1)
	}
}
