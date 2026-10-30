// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dav

// dav_meta_ops_test.go 覆盖 davMetaFS 装饰器的写/删/改名 sidecar 联动、可选能力转发与
// 降级语义（模块 §4「MetaPolicy 桩：错误传播」）：
//   - WriteMeta 失败只降级（不 fail-closed）而 SweepMetaTmp 恒被调用；
//   - 内层未实现可选能力 → ErrUnsupported（调用方自行回落）；
//   - 目录级 MOVE 迁移 sidecar 子树的失败路径 best-effort（Warn + 继续）。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// recordingMetaPolicy 记录 WriteMeta/SweepMetaTmp 调用并可注入 WriteMeta 错误。
type recordingMetaPolicy struct {
	writes []string
	sweeps []string
	err    error
}

func (p *recordingMetaPolicy) WriteMeta(ctx context.Context, owner string, root *storage.Root, rel string, extra map[string]any) error {
	p.writes = append(p.writes, rel)
	if p.err != nil {
		return p.err
	}
	return testMetaPolicy{}.WriteMeta(ctx, owner, root, rel, extra)
}

func (p *recordingMetaPolicy) SweepMetaTmp(root *storage.Root, mrel string) {
	p.sweeps = append(p.sweeps, mrel)
}

// noOptFS 只暴露 sync.FS 基础方法：隐藏内层可选能力，用于「内层未实现」分支。
type noOptFS struct{ syncpkg.FS }

// errOpFS 在底层 FS 上按需注入 Stat/WriteFile/Delete/Rename 错误。
type errOpFS struct {
	syncpkg.FS
	statErr   error
	writeErr  error
	deleteErr error
	renameErr error
}

func (e errOpFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if e.statErr != nil {
		return nil, e.statErr
	}
	return e.FS.Stat(ctx, p)
}

func (e errOpFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if e.writeErr != nil {
		return e.writeErr
	}
	return e.FS.WriteFile(ctx, key, p, r, size, mtime)
}

func (e errOpFS) Delete(ctx context.Context, p string) error {
	if e.deleteErr != nil {
		return e.deleteErr
	}
	return e.FS.Delete(ctx, p)
}

func (e errOpFS) Rename(ctx context.Context, from, to string) error {
	if e.renameErr != nil {
		return e.renameErr
	}
	return e.FS.Rename(ctx, from, to)
}

// stagingCapFS 记录 WithStagingQuota 注入的句柄。
type stagingCapFS struct {
	syncpkg.FS
	quota syncpkg.StagingQuotaTracker
}

func (s *stagingCapFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) { s.quota = q }

// stubStagingQuota 是 StagingQuotaTracker 的最小替身。
type stubStagingQuota struct{}

func (stubStagingQuota) ReserveUsage(context.Context, int64) error { return nil }
func (stubStagingQuota) ReleaseUsage(int64)                        {}

// exemptFS 声明 staging 配额豁免。
type exemptFS struct{ syncpkg.FS }

func (exemptFS) ExemptStagingQuota() bool { return true }

// reserveRecordFS 记录 ReserveSpace 调用。
type reserveRecordFS struct {
	syncpkg.FS
	got string
	err error
}

func (r *reserveRecordFS) ReserveSpace(_ context.Context, rel string, _ int64) error {
	r.got = rel
	return r.err
}

// directURLStub 提供直链能力。
type directURLStub struct {
	syncpkg.FS
	url string
}

func (d directURLStub) DirectURL(context.Context, string) (string, bool, error) {
	return d.url, true, nil
}

// metaExtraStub 记录 UpdateMetaExtra 调用。
type metaExtraStub struct {
	syncpkg.FS
	rel string
	err error
}

func (m *metaExtraStub) UpdateMetaExtra(_ context.Context, rel string, _ map[string]any) error {
	m.rel = rel
	return m.err
}

// nameListerStub 提供轻量列举能力。
type nameListerStub struct {
	syncpkg.FS
	called bool
	err    error
}

func (n *nameListerStub) ListDirNames(context.Context, string) ([]syncpkg.Entry, error) {
	n.called = true
	if n.err != nil {
		return nil, n.err
	}
	return []syncpkg.Entry{{Name: "a.txt", Path: "a.txt"}}, nil
}

// providerStub 提供 FileMeta 能力。
type providerStub struct {
	syncpkg.FS
	fm  *meta.FileMeta
	err error
}

func (p providerStub) FileMeta(context.Context, string) (*meta.FileMeta, error) { return p.fm, p.err }

// stagedReuseFS 以降级实现提供 WriteFromStagedPath（读入暂存文件后普通写）。
type stagedReuseFS struct {
	*syncpkg.LocalFS
	err error
}

func (s *stagedReuseFS) WriteFromStagedPath(ctx context.Context, rel, stagedPath string, _ int64, mtime int64) error {
	if s.err != nil {
		return s.err
	}
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		return err
	}
	return s.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(data), int64(len(data)), mtime)
}

// linkCopyFS 用「读源+写目标」实现 Link（不依赖底层硬链接支持，跨平台确定）。
type linkCopyFS struct{ *syncpkg.LocalFS }

func (l *linkCopyFS) Link(ctx context.Context, from, to string) error {
	rc, err := l.OpenRead(ctx, from)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return l.WriteFile(ctx, syncpkg.LogicalKey{}, to, bytes.NewReader(data), int64(len(data)), 0)
}

// writeIfAbsentErrFS 提供返回错误的 WriteIfAbsent 能力。
type writeIfAbsentErrFS struct{ syncpkg.FS }

func (writeIfAbsentErrFS) WriteIfAbsent(context.Context, string, io.Reader, int64, int64) (bool, error) {
	return false, errors.New("write if absent boom")
}

// newLocalDavMetaFS 构造「FS 根 = <dir>/user」的 davMetaFS（与既有包内测试同模型）。
func newLocalDavMetaFS(t *testing.T, dir string, root *storage.Root, policy MetaPolicy) *davMetaFS {
	t.Helper()
	userDir := filepath.Join(dir, "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", userDir, err)
	}
	return &davMetaFS{
		FS:    syncpkg.NewLocalFS(filepath.ToSlash(userDir), testLogger()),
		deps:  Deps{Logger: testLogger(), Meta: policy},
		root:  root,
		owner: "alice",
	}
}

// TestDavMetaFS_BucketRelAndLogger 覆盖路径归一与 logger 兜底。
func TestDavMetaFS_BucketRelAndLogger(t *testing.T) {
	t.Parallel()
	d := &davMetaFS{}
	for _, in := range []string{"", ".", "/", "./"} {
		if got := d.bucketRel(in); got != "" {
			t.Fatalf("bucketRel(%q) = %q, want 空", in, got)
		}
	}
	for in, want := range map[string]string{
		"a.txt":              "user/a.txt",
		"/a/b.txt":           "user/a/b.txt",
		"a/../../escape.txt": "user/escape.txt",
	} {
		if got := d.bucketRel(in); got != want {
			t.Fatalf("bucketRel(%q) = %q, want %q", in, got, want)
		}
	}
	if d.logger() == nil {
		t.Fatal("deps.Logger 为 nil 时应回落 slog.Default")
	}
	inner := syncpkg.NewLocalFS(t.TempDir(), testLogger())
	d2 := &davMetaFS{FS: inner}
	if d2.Inner() != inner {
		t.Fatal("Inner 应返回被包装 FS")
	}
}

// TestDavMetaFS_WithStagingQuota 覆盖 staging 配额注入：内层支持则转发，不支持则空操作。
func TestDavMetaFS_WithStagingQuota(t *testing.T) {
	t.Parallel()
	q := stubStagingQuota{}
	capable := &stagingCapFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}
	d := &davMetaFS{FS: capable}
	d.WithStagingQuota(q)
	if capable.quota != q {
		t.Fatal("内层实现 StagingQuotaCapable 时应收到注入")
	}
	// 内层不支持：不 panic、无副作用。
	(&davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}).WithStagingQuota(q)
}

// TestDavMetaFS_WriteMetaBranches 覆盖 writeMeta 的全部早退/降级分支：
// 空路径、目录、Stat 失败、未装配策略均不写；策略失败仅 Warn（写成功不 fail-closed）。
func TestDavMetaFS_WriteMetaBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("空路径", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.writeMeta(ctx, "")
		if len(policy.writes) != 0 {
			t.Fatalf("空路径不应写 sidecar: %v", policy.writes)
		}
	})

	t.Run("目录", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		if err := d.MakeDir(ctx, "docs"); err != nil {
			t.Fatalf("MakeDir: %v", err)
		}
		d.writeMeta(ctx, "docs")
		if len(policy.writes) != 0 {
			t.Fatalf("目录不应写 sidecar: %v", policy.writes)
		}
	})

	t.Run("Stat 失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.FS = errOpFS{FS: d.FS, statErr: errors.New("stat boom")}
		d.writeMeta(ctx, "a.txt")
		if len(policy.writes) != 0 {
			t.Fatalf("Stat 失败不应写 sidecar: %v", policy.writes)
		}
	})

	t.Run("未装配策略", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, nil)
		if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		d.writeMeta(ctx, "a.txt") // 不应 panic
	})

	t.Run("策略失败仅降级", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{err: errors.New("meta boom")}
		d := newLocalDavMetaFS(t, dir, root, policy)
		if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		d.writeMeta(ctx, "a.txt")
		if len(policy.writes) != 1 {
			t.Fatalf("策略应被调用一次, got %v", policy.writes)
		}
		if _, err := root.Stat(meta.MetaPath("user/a.txt")); err == nil {
			t.Fatal("策略失败时不应留下 sidecar")
		}
	})

	t.Run("策略成功", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		d.writeMeta(ctx, "a.txt")
		if _, err := root.Stat(meta.MetaPath("user/a.txt")); err != nil {
			t.Fatalf("策略成功后应有 sidecar: %v", err)
		}
	})
}

// TestDavMetaFS_DeleteCleansMetaAndReleasesQuota 覆盖 Delete 的 sidecar 清扫与 meta 桶配额释放
// （含 SweepMetaTmp 恒调用），以及「无配额作用域 / root 未装配」的安全跳过。
func TestDavMetaFS_DeleteCleansMetaAndReleasesQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("释放配额并清扫", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		pool := quota.NewPool(0)
		scope := pool.Scope("meta", 0)
		res, err := scope.TryReserve(100)
		if err != nil {
			t.Fatalf("TryReserve: %v", err)
		}
		res.Commit(100)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		var gotRel string
		d.deps.QuotaScopeFor = func(_, rel string) *quota.Scope {
			gotRel = rel
			return scope
		}
		if werr := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
		d.writeMeta(ctx, "a.txt")

		if derr := d.Delete(ctx, "a.txt"); derr != nil {
			t.Fatalf("Delete: %v", derr)
		}
		mrel := meta.MetaPath("user/a.txt")
		if gotRel != mrel {
			t.Fatalf("QuotaScopeFor rel = %q, want %q", gotRel, mrel)
		}
		if len(policy.sweeps) != 1 || policy.sweeps[0] != mrel {
			t.Fatalf("SweepMetaTmp 应以 %q 调用一次, got %v", mrel, policy.sweeps)
		}
		if _, serr := root.Stat(mrel); serr == nil {
			t.Fatal("Delete 后 sidecar 应已删除")
		}
		if got := scope.Usage(); got != 98 {
			t.Fatalf("sidecar 2 字节应释放, Usage = %d, want 98", got)
		}
	})

	t.Run("配额作用域为 nil", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.deps.QuotaScopeFor = func(string, string) *quota.Scope { return nil }
		if werr := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
		d.writeMeta(ctx, "a.txt")
		if derr := d.Delete(ctx, "a.txt"); derr != nil {
			t.Fatalf("Delete: %v", derr)
		}
	})

	t.Run("root 未装配", func(t *testing.T) {
		t.Parallel()
		_, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, nil, &recordingMetaPolicy{})
		if werr := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
		if derr := d.Delete(ctx, "a.txt"); derr != nil {
			t.Fatalf("Delete: %v", derr)
		}
	})

	t.Run("内层删除失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.FS = errOpFS{FS: d.FS, deleteErr: errors.New("delete boom")}
		if derr := d.Delete(ctx, "a.txt"); derr == nil {
			t.Fatal("内层删除失败应向上返回")
		}
	})

	t.Run("空路径无副作用", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.removeMeta("")
		if len(policy.sweeps) != 0 {
			t.Fatalf("空路径不应清扫: %v", policy.sweeps)
		}
	})
}

// TestDavMetaFS_RenameError 内层改名失败时直接上抛，不做 sidecar 重链。
func TestDavMetaFS_RenameError(t *testing.T) {
	t.Parallel()
	root, dir := newTestRoot(t)
	policy := &recordingMetaPolicy{}
	d := newLocalDavMetaFS(t, dir, root, policy)
	d.FS = errOpFS{FS: d.FS, renameErr: errors.New("rename boom")}
	if err := d.Rename(context.Background(), "a.txt", "b.txt"); err == nil {
		t.Fatal("内层改名失败应向上返回")
	}
	if len(policy.writes) != 0 || len(policy.sweeps) != 0 {
		t.Fatalf("失败不应触发重链: writes=%v sweeps=%v", policy.writes, policy.sweeps)
	}
}

// TestDavMetaFS_WriteFileFallback 覆盖未装配 FileService 时的内层直写 + sidecar 联动，
// 以及内层写失败（不写 sidecar）与 FileService 返回 nil 的回落。
func TestDavMetaFS_WriteFileFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := syncpkg.LogicalKey{}

	t.Run("直写成功建 sidecar", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		if err := d.WriteFile(ctx, key, "a.txt", strings.NewReader("hello"), 5, 0); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := root.Stat(meta.MetaPath("user/a.txt")); err != nil {
			t.Fatalf("直写后应有 sidecar: %v", err)
		}
	})

	t.Run("内层写失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.FS = errOpFS{FS: d.FS, writeErr: errors.New("write boom")}
		if err := d.WriteFile(ctx, key, "a.txt", strings.NewReader("hello"), 5, 0); err == nil {
			t.Fatal("内层写失败应向上返回")
		}
		if len(policy.writes) != 0 {
			t.Fatalf("写失败不应写 sidecar: %v", policy.writes)
		}
	})

	t.Run("FileService 返回 nil 回落直写", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, &recordingMetaPolicy{})
		d.deps.FileService = func() *files.Service { return nil }
		if err := d.WriteFile(ctx, key, "a.txt", strings.NewReader("hello"), 5, 0); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	})
}

// TestDavMetaFS_WriteFileThroughFileService 覆盖统一写内核分支：合法路径落盘；
// 非法远端路径（空）由内核拒绝并原样上抛。
func TestDavMetaFS_WriteFileThroughFileService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, dir := newTestRoot(t)
	cache := storage.NewTenantCache(root, storage.WithMetaBucket())
	t.Cleanup(func() { _ = cache.Close() })
	svc, err := files.New(cache, files.WithLogger(testLogger))
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	d := &davMetaFS{
		FS:    syncpkg.NewLocalFS(filepath.ToSlash(filepath.Join(dir, "unused")), testLogger()),
		deps:  Deps{Logger: testLogger(), FileService: func() *files.Service { return svc }},
		owner: "alice",
		root:  root,
	}

	if werr := d.WriteFile(ctx, syncpkg.LogicalKey{}, "docs/a.txt", strings.NewReader("kernel"), 6, 0); werr != nil {
		t.Fatalf("统一写内核应成功: %v", werr)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "alice", "user", "docs", "a.txt"))
	if rerr != nil || string(got) != "kernel" {
		t.Fatalf("内容 = %q err=%v", got, rerr)
	}

	// 空路径 → WriteTarget 解析失败（fail-closed），错误上抛。
	if werr := d.WriteFile(ctx, syncpkg.LogicalKey{}, "", strings.NewReader("x"), 1, 0); werr == nil {
		t.Fatal("空远端路径应返回错误")
	}
}

// TestDavMetaFS_WriteIfAbsent 覆盖唯一写能力：内层未实现 / 已存在（不重链）/ 成功重链 / 底层错误。
func TestDavMetaFS_WriteIfAbsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("内层未实现", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if _, err := d.WriteIfAbsent(ctx, "a.txt", strings.NewReader("x"), 1, 0); !errors.Is(err, syncpkg.ErrUnsupported) {
			t.Fatalf("应返回 ErrUnsupported, got %v", err)
		}
	})

	t.Run("已存在不重链", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("old"), 3, 0); err != nil {
			t.Fatalf("预置: %v", err)
		}
		written, err := d.WriteIfAbsent(ctx, "a.txt", strings.NewReader("new"), 3, 0)
		if err != nil || written {
			t.Fatalf("已存在应 (false,nil), got (%v,%v)", written, err)
		}
		if len(policy.writes) != 0 {
			t.Fatalf("未写入不应重链 sidecar: %v", policy.writes)
		}
	})

	t.Run("成功重链", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		written, err := d.WriteIfAbsent(ctx, "a.txt", strings.NewReader("new"), 3, 0)
		if err != nil || !written {
			t.Fatalf("首次应 (true,nil), got (%v,%v)", written, err)
		}
		if _, serr := root.Stat(meta.MetaPath("user/a.txt")); serr != nil {
			t.Fatalf("应重建 sidecar: %v", serr)
		}
	})

	t.Run("底层错误", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: writeIfAbsentErrFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if _, err := d.WriteIfAbsent(ctx, "a.txt", strings.NewReader("x"), 1, 0); err == nil {
			t.Fatal("底层错误应向上返回")
		}
	})
}

// TestDavMetaFS_WriteFromStagedPath 覆盖暂存路径复用：未实现 / 成功重链 / 底层失败。
func TestDavMetaFS_WriteFromStagedPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("内层未实现", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if err := d.WriteFromStagedPath(ctx, "a.txt", "staged", 1, 0); !errors.Is(err, syncpkg.ErrUnsupported) {
			t.Fatalf("应返回 ErrUnsupported, got %v", err)
		}
	})

	t.Run("成功重链", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		staged := filepath.Join(t.TempDir(), "staged.bin")
		if err := os.WriteFile(staged, []byte("staged"), 0o644); err != nil {
			t.Fatalf("暂存文件: %v", err)
		}
		policy := &recordingMetaPolicy{}
		d := newLocalDavMetaFS(t, dir, root, policy)
		d.FS = &stagedReuseFS{LocalFS: d.FS.(*syncpkg.LocalFS)}
		if err := d.WriteFromStagedPath(ctx, "a.txt", staged, 6, 0); err != nil {
			t.Fatalf("WriteFromStagedPath: %v", err)
		}
		if _, serr := root.Stat(meta.MetaPath("user/a.txt")); serr != nil {
			t.Fatalf("应重建 sidecar: %v", serr)
		}
	})

	t.Run("底层失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, &recordingMetaPolicy{})
		d.FS = &stagedReuseFS{LocalFS: d.FS.(*syncpkg.LocalFS), err: errors.New("staged boom")}
		if err := d.WriteFromStagedPath(ctx, "a.txt", "x", 1, 0); err == nil {
			t.Fatal("底层失败应向上返回")
		}
	})
}

// TestDavMetaFS_MoveCopyLink 覆盖移动/复制/链接的「未实现 → ErrUnsupported / 成功重链 /
// 内层失败」三条分支。写面能力必须带 meta 重链（否则目标读侧零校验）。
func TestDavMetaFS_MoveCopyLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	type opCase struct {
		name string
		call func(d *davMetaFS, from, to string) error
		// newFS 返回带该能力的底层 FS；nil = 默认 LocalFS。
		newFS func(base *syncpkg.LocalFS) syncpkg.FS
	}
	cases := []opCase{
		{
			name:  "Move",
			call:  func(d *davMetaFS, from, to string) error { return d.Move(ctx, from, to) },
			newFS: func(base *syncpkg.LocalFS) syncpkg.FS { return base },
		},
		{
			name:  "Copy",
			call:  func(d *davMetaFS, from, to string) error { return d.Copy(ctx, from, to) },
			newFS: func(base *syncpkg.LocalFS) syncpkg.FS { return base },
		},
		{
			name:  "Link",
			call:  func(d *davMetaFS, from, to string) error { return d.Link(ctx, from, to) },
			newFS: func(base *syncpkg.LocalFS) syncpkg.FS { return &linkCopyFS{LocalFS: base} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_内层未实现", func(t *testing.T) {
			t.Parallel()
			d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
			if err := tc.call(d, "a.txt", "b.txt"); !errors.Is(err, syncpkg.ErrUnsupported) {
				t.Fatalf("应返回 ErrUnsupported, got %v", err)
			}
		})
		t.Run(tc.name+"_成功重链", func(t *testing.T) {
			t.Parallel()
			root, dir := newTestRoot(t)
			policy := &recordingMetaPolicy{}
			d := newLocalDavMetaFS(t, dir, root, policy)
			base := d.FS.(*syncpkg.LocalFS)
			d.FS = tc.newFS(base)
			if werr := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); werr != nil {
				t.Fatalf("预置写: %v", werr)
			}
			d.writeMeta(ctx, "a.txt")
			if err := tc.call(d, "a.txt", "b.txt"); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if _, serr := root.Stat(meta.MetaPath("user/b.txt")); serr != nil {
				t.Fatalf("目标应有 sidecar: %v", serr)
			}
		})
		t.Run(tc.name+"_内层失败", func(t *testing.T) {
			t.Parallel()
			root, dir := newTestRoot(t)
			d := newLocalDavMetaFS(t, dir, root, &recordingMetaPolicy{})
			d.FS = tc.newFS(d.FS.(*syncpkg.LocalFS))
			if err := tc.call(d, "missing.txt", "b.txt"); err == nil {
				t.Fatalf("%s 源不存在应返回错误", tc.name)
			}
		})
	}
}

// TestDavMetaFS_ReserveAndExempt 覆盖容量预检与 staging 豁免能力转发（含未实现默认值）。
func TestDavMetaFS_ReserveAndExempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ReserveSpace 未实现", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if err := d.ReserveSpace(ctx, "a", 1); !errors.Is(err, syncpkg.ErrUnsupported) {
			t.Fatalf("应返回 ErrUnsupported, got %v", err)
		}
	})

	t.Run("ReserveSpace 转发成功与失败", func(t *testing.T) {
		t.Parallel()
		stub := &reserveRecordFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}
		d := &davMetaFS{FS: stub}
		if err := d.ReserveSpace(ctx, "a.txt", 10); err != nil {
			t.Fatalf("ReserveSpace: %v", err)
		}
		if stub.got != "a.txt" {
			t.Fatalf("转发 rel = %q", stub.got)
		}
		stub.err = errors.New("reserve boom")
		if err := d.ReserveSpace(ctx, "a.txt", 10); err == nil {
			t.Fatal("底层错误应向上返回")
		}
	})

	t.Run("IsLocalVolume 默认外部", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if d.IsLocalVolume() {
			t.Fatal("未实现 LocalVolume 的底层应视为外部卷（false）")
		}
	})

	t.Run("ExemptStagingQuota", func(t *testing.T) {
		t.Parallel()
		exempt := &davMetaFS{FS: exemptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if !exempt.ExemptStagingQuota() {
			t.Fatal("内层声明豁免时应转发 true")
		}
		plain := &davMetaFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}
		if plain.ExemptStagingQuota() {
			t.Fatal("未实现应返回 false（默认强制 gate）")
		}
	})
}

// TestDavMetaFS_OpenRangeRead 覆盖区间读：成功转发与未实现。
func TestDavMetaFS_OpenRangeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, dir := newTestRoot(t)
	d := newLocalDavMetaFS(t, dir, root, nil)
	if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("0123456789"), 10, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := d.OpenRangeRead(ctx, "a.txt", 2, 3)
	if err != nil {
		t.Fatalf("OpenRangeRead: %v", err)
	}
	data, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil || string(data) != "234" {
		t.Fatalf("区间读 = %q err=%v", data, rerr)
	}

	unsupported := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
	if _, oerr := unsupported.OpenRangeRead(ctx, "a.txt", 0, 1); !errors.Is(oerr, syncpkg.ErrUnsupported) {
		t.Fatalf("应返回 ErrUnsupported, got %v", oerr)
	}
}

// TestDavMetaFS_DirectURL 覆盖直链：封印路径必须抑制、未实现透传错误、已实现转发。
func TestDavMetaFS_DirectURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("封印路径抑制直链", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		if err := root.SetSeals([]storage.SealRule{{Path: "user/vault", Wrapper: "secretdata"}}); err != nil {
			t.Fatalf("SetSeals: %v", err)
		}
		d := newLocalDavMetaFS(t, dir, root, nil)
		d.FS = directURLStub{FS: d.FS, url: "https://example.com/x"}
		url, ok, err := d.DirectURL(ctx, "vault/x.bin")
		if err != nil || ok || url != "" {
			t.Fatalf("封印路径应抑制直链: url=%q ok=%v err=%v", url, ok, err)
		}
	})

	t.Run("未实现", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
		if _, _, err := d.DirectURL(ctx, "a.txt"); !errors.Is(err, syncpkg.ErrUnsupported) {
			t.Fatalf("应返回 ErrUnsupported, got %v", err)
		}
	})

	t.Run("转发成功", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{FS: directURLStub{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger()), url: "https://example.com/a"}}
		url, ok, err := d.DirectURL(ctx, "a.txt")
		if err != nil || !ok || url != "https://example.com/a" {
			t.Fatalf("转发 = (%q,%v,%v)", url, ok, err)
		}
	})
}

// TestDavMetaFS_UpdateMetaExtra 覆盖旁路 Extra 更新：未实现与转发（含底层错误）。
func TestDavMetaFS_UpdateMetaExtra(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	unsupported := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
	if err := unsupported.UpdateMetaExtra(ctx, "a.txt", nil); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("应返回 ErrUnsupported, got %v", err)
	}

	stub := &metaExtraStub{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}
	d := &davMetaFS{FS: stub}
	if err := d.UpdateMetaExtra(ctx, "a.txt", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("UpdateMetaExtra: %v", err)
	}
	if stub.rel != "a.txt" {
		t.Fatalf("转发 rel = %q", stub.rel)
	}
	stub.err = errors.New("extra boom")
	if err := d.UpdateMetaExtra(ctx, "a.txt", nil); err == nil {
		t.Fatal("底层错误应向上返回")
	}
}

// TestDavMetaFS_FileMeta 覆盖 sidecar 读取转发：未实现与底层错误。
func TestDavMetaFS_FileMeta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	unsupported := &davMetaFS{FS: noOptFS{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}}
	if _, err := unsupported.FileMeta(ctx, "a.txt"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("应返回 ErrUnsupported, got %v", err)
	}

	fm := &meta.FileMeta{Version: 1, Size: 1}
	d := &davMetaFS{FS: providerStub{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger()), fm: fm}}
	if got, err := d.FileMeta(ctx, "a.txt"); err != nil || got != fm {
		t.Fatalf("FileMeta = (%v,%v)", got, err)
	}
	d.FS = providerStub{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger()), err: errors.New("meta boom")}
	if _, err := d.FileMeta(ctx, "a.txt"); err == nil {
		t.Fatal("底层错误应向上返回")
	}
}

// TestDavMetaFS_ListDirNames 覆盖轻量列举：有 NameLister 则转发，否则回落 ListDir。
func TestDavMetaFS_ListDirNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, dir := newTestRoot(t)
	d := newLocalDavMetaFS(t, dir, root, nil)
	if err := d.FS.WriteFile(ctx, syncpkg.LogicalKey{}, "a.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// LocalFS 未实现 NameLister → 回落 ListDir。
	es, err := d.ListDirNames(ctx, "")
	if err != nil || len(es) != 1 || es[0].Name != "a.txt" {
		t.Fatalf("回落 ListDir = %v err=%v", es, err)
	}

	stub := &nameListerStub{FS: syncpkg.NewLocalFS(t.TempDir(), testLogger())}
	d.FS = stub
	if es, err := d.ListDirNames(ctx, ""); err != nil || len(es) != 1 || !stub.called {
		t.Fatalf("应转发 NameLister: es=%v called=%v err=%v", es, stub.called, err)
	}
	stub.err = errors.New("list boom")
	if _, err := d.ListDirNames(ctx, ""); err == nil {
		t.Fatal("底层错误应向上返回")
	}
}

// TestDavMetaFS_MigrateMetaSubtreeBranches 覆盖目录级 sidecar 子树迁移的边界与 best-effort
// 失败路径（root 未装配 / 空路径 / 同目录 / 源缺失 / 目标目录建立失败 / 改名失败）。
func TestDavMetaFS_MigrateMetaSubtreeBranches(t *testing.T) {
	t.Parallel()

	t.Run("root 未装配", func(t *testing.T) {
		t.Parallel()
		d := &davMetaFS{}
		if got := d.migrateMetaSubtree("docs", "docs2"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})

	t.Run("空路径与同目录", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, nil)
		if got := d.migrateMetaSubtree("", "docs2"); got != 0 {
			t.Fatalf("空源: got %d", got)
		}
		if got := d.migrateMetaSubtree("docs", ""); got != 0 {
			t.Fatalf("空目标: got %d", got)
		}
		if got := d.migrateMetaSubtree("a", "a/"); got != 0 {
			t.Fatalf("同目录: got %d", got)
		}
	})

	t.Run("源子树不存在", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, nil)
		if got := d.migrateMetaSubtree("nope", "other"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})

	t.Run("目标目录建立失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, nil)
		if err := writeRootFile(t, dir, "meta/docs/a.txt.meta"); err != nil {
			t.Fatalf("预置 sidecar: %v", err)
		}
		// 目标子树目录被同名文件占用 → MkdirAll 失败（Warn 后继续，不计入迁移条数）。
		if err := writeRootFile(t, dir, "meta/docs2"); err != nil {
			t.Fatalf("占位目标: %v", err)
		}
		if got := d.migrateMetaSubtree("docs", "docs2"); got != 0 {
			t.Fatalf("建立失败应跳过, got %d", got)
		}
	})

	t.Run("改名失败", func(t *testing.T) {
		t.Parallel()
		root, dir := newTestRoot(t)
		d := newLocalDavMetaFS(t, dir, root, nil)
		if err := writeRootFile(t, dir, "meta/docs/a.txt.meta"); err != nil {
			t.Fatalf("预置 sidecar: %v", err)
		}
		// 目标父目录可建，但目标名被同名**目录**占用 → Rename 失败。
		if err := os.MkdirAll(filepath.Join(dir, "meta", "docs2", "a.txt.meta"), 0o755); err != nil {
			t.Fatalf("占位目标目录: %v", err)
		}
		if got := d.migrateMetaSubtree("docs", "docs2"); got != 0 {
			t.Fatalf("改名失败应跳过, got %d", got)
		}
		if _, err := os.Stat(filepath.Join(dir, "meta", "docs", "a.txt.meta")); err != nil {
			t.Fatalf("源 sidecar 应保留（失败 best-effort 不删除）: %v", err)
		}
	})
}

// writeRootFile 在 <dir>/<rel> 写一个占位文件（自动建父目录）。
func writeRootFile(t *testing.T, dir, rel string) error {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte("{}"), 0o644)
}
