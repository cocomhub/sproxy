// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestWalkRoot_Recursive 递归统计目录/文件/字节，跳过 .__ 魔法目录与元数据文件。
func TestWalkRoot_Recursive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, p := range []string{"user/a.txt", "user/sub/b.txt", "user/.__trash/c.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte("12345"), 0o644); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
	}
	if wErr := os.WriteFile(filepath.Join(dir, "user", "checksums.json"), []byte("{}"), 0o644); wErr != nil {
		t.Fatalf("write meta: %v", wErr)
	}

	d, err := WalkRoot(root, "user")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	if d.Path != "user" || d.Files != 2 || d.Dirs != 1 || d.Size != 10 {
		t.Fatalf("got %+v, want path=user files=2 dirs=1 size=10", d)
	}
}

// TestWalkFS_MatchesRoot 同一棵树经 FS 抽象与 Root 得到一致统计。
func TestWalkFS_MatchesRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if mkErr := os.MkdirAll(filepath.Join(dir, "user", "d"), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(dir, "user", "d", "f.bin"), []byte("abc"), 0o644); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}

	want, err := WalkRoot(root, "user")
	if err != nil {
		t.Fatalf("WalkRoot: %v", err)
	}
	got, err := WalkFS(context.Background(), syncpkg.NewLocalFS(dir, slog.New(slog.DiscardHandler)), "user")
	if err != nil {
		t.Fatalf("WalkFS: %v", err)
	}
	if got.Files != want.Files || got.Dirs != want.Dirs || got.Size != want.Size {
		t.Fatalf("FS 与 Root 统计不一致: got %+v want %+v", got, want)
	}
}

// TestWalkUploadStats_CategorySplit 分类遍历：user/cloud/chunk/version 分桶、meta 与
// 内部目录跳过。
func TestWalkUploadStats_CategorySplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{
		"user/a.txt", "cloud/t/f.bin", "archive/t/f.bin",
		"chunk/s/0.part", "version/doc/v1", "meta/state.json",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte("1234"), 0o644); wErr != nil {
			t.Fatalf("write: %v", wErr)
		}
	}
	uf, ch, ve, cl := WalkUploadStatsByCategory(dir, slog.New(slog.DiscardHandler))
	if uf != 4 || cl != 8 || ch != 4 || ve != 4 {
		t.Fatalf("分类用量 = user=%d cloud=%d chunk=%d version=%d，want 4/8/4/4", uf, cl, ch, ve)
	}
	files, size := WalkUploadStats(dir, slog.New(slog.DiscardHandler))
	if files != 1 || size != 4 {
		t.Fatalf("WalkUploadStats = %d files / %d bytes，want 1/4（只统计 user 桶）", files, size)
	}
}

// TestCategoriesFromBuckets 聚合 path→bytes（末段桶名；cloud+archive 合并）。
func TestCategoriesFromBuckets(t *testing.T) {
	t.Parallel()
	uf, cl, ch, ve := CategoriesFromBuckets(map[string]int64{
		"alice/user": 10, "alice/cloud": 20, "alice/archive": 5,
		"alice/chunk": 3, "alice/version": 2, "alice/meta": 99,
	})
	if uf != 10 || cl != 25 || ch != 3 || ve != 2 {
		t.Fatalf("got %d/%d/%d/%d，want 10/25/3/2", uf, cl, ch, ve)
	}
}

// fakeFS 是 syncpkg.FS 的最小替身：只实现 ListDir（WalkFS 只用它），其余方法保留嵌入接口
// 的 nil 实现（误用即 panic，说明用例走偏）。
type fakeFS struct {
	syncpkg.FS
	dirs map[string][]syncpkg.Entry
	errs map[string]error
}

func (f *fakeFS) ListDir(_ context.Context, key string) ([]syncpkg.Entry, error) {
	if lerr, ok := f.errs[key]; ok {
		return nil, lerr
	}
	entries, ok := f.dirs[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return entries, nil
}

// fakeDirEntry 是 os.DirEntry 的最小替身：用于构造真实文件系统难以触发的 Info 失败。
type fakeDirEntry struct {
	name    string
	dir     bool
	infoErr error
	size    int64
}

func (e fakeDirEntry) Name() string { return e.name }
func (e fakeDirEntry) IsDir() bool  { return e.dir }

func (e fakeDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}

func (e fakeDirEntry) Info() (os.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return fakeFileInfo{size: e.size}, nil
}

// fakeFileInfo 是 os.FileInfo 的最小替身（仅 Size 有意义）。
type fakeFileInfo struct{ size int64 }

func (f fakeFileInfo) Name() string       { return "f" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// TestWalkFS_ListDirErrors 列出失败：目录不存在 = 空统计（不报错），其它错误 fail-closed
// 返回错误（不交付部分结果）；子目录错误经 walkFSEntry 向上传播。
func TestWalkFS_ListDirErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("listdir boom")
	cases := []struct {
		name    string
		fsys    *fakeFS
		wantErr error
	}{
		{
			name: "根目录不存在=空统计",
			fsys: &fakeFS{},
		},
		{
			name:    "根目录读取失败",
			fsys:    &fakeFS{errs: map[string]error{"user": boom}},
			wantErr: boom,
		},
		{
			name: "子目录读取失败",
			fsys: &fakeFS{
				dirs: map[string][]syncpkg.Entry{"user": {{Name: "sub", IsDir: true}}},
				errs: map[string]error{"user/sub": boom},
			},
			wantErr: boom,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WalkFS(context.Background(), tc.fsys, "user")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("WalkFS 错误 = %v，want %v", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("失败时不应交付部分结果，got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("WalkFS: %v", err)
			}
			if *got != (Data{}) {
				t.Fatalf("空目录统计 = %+v，want 零值", got)
			}
		})
	}
}

// TestWalkFS_SkipsMagicDirAndMetadata `.__` 魔法目录不计入目录数、也不再递归；
// checksums.json / LAYOUT_VERSION 元数据文件不计入文件数/字节。
func TestWalkFS_SkipsMagicDirAndMetadata(t *testing.T) {
	t.Parallel()
	fsys := &fakeFS{dirs: map[string][]syncpkg.Entry{
		"user": {
			{Name: ".__tmp", IsDir: true},
			{Name: "d", IsDir: true},
			{Name: "checksums.json", Size: 100},
			{Name: "LAYOUT_VERSION", Size: 100},
			{Name: "keep.txt", Size: 7},
		},
		"user/d":      {{Name: "x.bin", Size: 3}},
		"user/.__tmp": {{Name: "hidden.bin", Size: 1000}},
	}}
	got, err := WalkFS(context.Background(), fsys, "user")
	if err != nil {
		t.Fatalf("WalkFS: %v", err)
	}
	if got.Dirs != 1 || got.Files != 2 || got.Size != 10 {
		t.Fatalf("got %+v，want dirs=1 files=2 size=10", got)
	}
}

// TestWalkRoot_MissingAndEscape 目录不存在 = 空统计；越界路径（非 NotExist 错误）
// fail-closed 返回错误（不交付部分结果）。
func TestWalkRoot_MissingAndEscape(t *testing.T) {
	t.Parallel()
	root, err := storage.OpenRoot(testutil.MkdirTemp(t, "sproxy-ops-root-"))
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	got, err := WalkRoot(root, "absent")
	if err != nil {
		t.Fatalf("WalkRoot(不存在): %v", err)
	}
	if got.Path != "absent" || got.Dirs != 0 || got.Files != 0 || got.Size != 0 {
		t.Fatalf("got %+v，want path=absent 全零计数", got)
	}

	got, err = WalkRoot(root, "../outside")
	if err == nil {
		t.Fatalf("越界路径应 fail-closed，got %+v", got)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("越界路径错误不应被当成「目录不存在」: %v", err)
	}
	if got != nil {
		t.Fatalf("失败时不应交付部分结果，got %+v", got)
	}
}

// TestWalkEntry_InfoError 文件 Info 失败 ⇒ 错误向上传播（不静默跳过）。
func TestWalkEntry_InfoError(t *testing.T) {
	t.Parallel()
	root, err := storage.OpenRoot(testutil.MkdirTemp(t, "sproxy-ops-entry-"))
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	infoErr := errors.New("info boom")
	d := &Data{}
	if werr := walkEntry(root, "user", fakeDirEntry{name: "f.txt", infoErr: infoErr}, d); !errors.Is(werr, infoErr) {
		t.Fatalf("walkEntry 错误 = %v，want %v", werr, infoErr)
	}
	if d.Files != 0 || d.Size != 0 {
		t.Fatalf("Info 失败不应计入统计，got %+v", d)
	}
}

// TestBucketOf 桶归属：租户根相对路径取首段（已知桶名优先）、存储根相对路径取第 2 段；
// 单段未知返回空（回退 capacity.StorageBucketOf 不校验桶名，故第 2 段原样返回）。
func TestBucketOf(t *testing.T) {
	t.Parallel()
	cases := []struct{ rel, want string }{
		{"user", "user"},
		{"user/f.txt", "user"},
		{"version/doc/v1", "version"},
		{"alice/user/f.txt", "user"},
		{"alice/meta", "meta"},
		{"alice/backup/manifest.json", "backup"},
		{"alice/nosuchbucket", "nosuchbucket"},
		// 旧布局平铺文件：第 2 段即文件名（非桶名），调用方靠 IsStorageBucket 判定。
		{"alice/f.txt", "f.txt"},
		{"f.txt", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := BucketOf(tc.rel); got != tc.want {
			t.Errorf("BucketOf(%q) = %q，want %q", tc.rel, got, tc.want)
		}
	}
}

// TestIsStorageBucket 新布局功能桶名判定（含 backup 与未知/大小写不敏感为 false）。
func TestIsStorageBucket(t *testing.T) {
	t.Parallel()
	for _, bucket := range []string{"user", "cloud", "archive", "chunk", "version", "meta", "backup"} {
		if !IsStorageBucket(bucket) {
			t.Errorf("IsStorageBucket(%q) = false，want true", bucket)
		}
	}
	for _, bucket := range []string{"", "USER", "unknown", "t"} {
		if IsStorageBucket(bucket) {
			t.Errorf("IsStorageBucket(%q) = true，want false", bucket)
		}
	}
}

// TestWalkUploadStats_NotFound 遍历根不存在：WalkDir 错误回调只记 Warn，不 panic 且返回零值。
func TestWalkUploadStats_NotFound(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "sproxy-ops-stats-")
	missing := filepath.Join(dir, "missing")
	logger := slog.New(slog.DiscardHandler)
	if files, size := WalkUploadStats(missing, logger); files != 0 || size != 0 {
		t.Fatalf("WalkUploadStats = %d/%d，want 0/0", files, size)
	}
	uf, ch, ve, cl := WalkUploadStatsByCategory(missing, logger)
	if uf != 0 || ch != 0 || ve != 0 || cl != 0 {
		t.Fatalf("ByCategory = %d/%d/%d/%d，want 全 0", uf, ch, ve, cl)
	}
}

// TestWalkUploadStats_Skips 只统计 user 桶与旧布局平铺文件；其它功能桶、`.__`/`__` 内部
// 目录、checksums.json / LAYOUT_VERSION 元数据都不计入。
func TestWalkUploadStats_Skips(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "sproxy-ops-upload-")
	for _, p := range []string{
		"user/a.txt", "old.txt", "checksums.json", "LAYOUT_VERSION",
		".__trash/hidden.bin", "__internal/state.bin",
		"cloud/f.bin", "meta/state.json", "backup/manifest.json",
	} {
		writeFixtureFile(t, dir, p, "1234")
	}
	files, size := WalkUploadStats(dir, slog.New(slog.DiscardHandler))
	// 只有 user/a.txt 与旧布局 old.txt 计入（各 4 字节）。
	if files != 2 || size != 8 {
		t.Fatalf("WalkUploadStats = %d files / %d bytes，want 2/8", files, size)
	}
}

// TestWalkUploadStats_FlatBucketNamedFiles 旧布局平铺区里「文件名恰好等于功能桶名」的
// 文件按非 user 桶处理（目录层跳过分支在平铺文件上不适用，需文件层判定兼容）。
func TestWalkUploadStats_FlatBucketNamedFiles(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "sproxy-ops-flat-")
	for _, p := range []string{"cloud", "meta", "backup", "old.txt"} {
		writeFixtureFile(t, dir, p, "1234")
	}
	files, size := WalkUploadStats(dir, slog.New(slog.DiscardHandler))
	if files != 1 || size != 4 {
		t.Fatalf("WalkUploadStats = %d files / %d bytes，want 1/4（只计 old.txt）", files, size)
	}
}

// TestWalkUploadStatsByCategory_Skips meta/backup 桶与 `.__`/`__` 目录不计入任何分类；
// checksums.json 跳过；无桶结构的旧布局平铺文件归入 user。
func TestWalkUploadStatsByCategory_Skips(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "sproxy-ops-category-")
	for _, p := range []string{
		"user/a.txt", "old.txt", "checksums.json",
		".__trash/hidden.bin", "__internal/state.bin",
		"meta/state.json", "backup/manifest.json",
	} {
		writeFixtureFile(t, dir, p, "1234")
	}
	uf, ch, ve, cl := WalkUploadStatsByCategory(dir, slog.New(slog.DiscardHandler))
	if uf != 8 || ch != 0 || ve != 0 || cl != 0 {
		t.Fatalf("ByCategory = user=%d chunk=%d version=%d cloud=%d，want 8/0/0/0", uf, ch, ve, cl)
	}
}

// TestWalkUploadStatsByCategory_FlatBucketNamedFiles 平铺区「桶名文件」走文件层兜底分支：
// meta/backup 不计入任何分类，其余平铺文件归入 user。
func TestWalkUploadStatsByCategory_FlatBucketNamedFiles(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "sproxy-ops-category-flat-")
	for _, p := range []string{"meta", "backup", "old.txt"} {
		writeFixtureFile(t, dir, p, "1234")
	}
	uf, ch, ve, cl := WalkUploadStatsByCategory(dir, slog.New(slog.DiscardHandler))
	if uf != 4 || ch != 0 || ve != 0 || cl != 0 {
		t.Fatalf("ByCategory = user=%d chunk=%d version=%d cloud=%d，want 4/0/0/0", uf, ch, ve, cl)
	}
}

// TestWalkUploadStatsEntry_RelAndInfoErrors 路径无法相对化（root 相对 + path 绝对）与文件
// Info 失败都只跳过该条目，不中断遍历、不计入统计。
func TestWalkUploadStatsEntry_RelAndInfoErrors(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	absPath := filepath.Join(os.TempDir(), "f.txt")

	statsAcc := &uploadStatsAccumulator{}
	if err := walkUploadStatsEntry(logger, "relative-root", absPath, fakeDirEntry{name: "f.txt"}, nil, statsAcc); err != nil {
		t.Fatalf("Rel 失败应跳过并返回 nil，got %v", err)
	}
	if statsAcc.files != 0 || statsAcc.size != 0 {
		t.Fatalf("Rel 失败不应计入统计，got %+v", statsAcc)
	}
	infoErr := errors.New("info boom")
	if err := walkUploadStatsEntry(logger, "relative-root", "relative-root/f.txt",
		fakeDirEntry{name: "f.txt", infoErr: infoErr}, nil, statsAcc); err != nil {
		t.Fatalf("Info 失败应跳过并返回 nil，got %v", err)
	}
	if statsAcc.files != 0 || statsAcc.size != 0 {
		t.Fatalf("Info 失败不应计入统计，got %+v", statsAcc)
	}

	catAcc := &uploadCategoryAccum{}
	if err := walkUploadStatsCategoryEntry(logger, "relative-root", absPath, fakeDirEntry{name: "f.txt"}, nil, catAcc); err != nil {
		t.Fatalf("Rel 失败应跳过并返回 nil，got %v", err)
	}
	if err := walkUploadStatsCategoryEntry(logger, "relative-root", "relative-root/f.txt",
		fakeDirEntry{name: "f.txt", infoErr: infoErr}, nil, catAcc); err != nil {
		t.Fatalf("Info 失败应跳过并返回 nil，got %v", err)
	}
	if *catAcc != (uploadCategoryAccum{}) {
		t.Fatalf("失败条目不应计入分类，got %+v", catAcc)
	}
}

// writeFixtureFile 写入测试 fixture 文件（自动建父目录）。
func writeFixtureFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestQuotaStatusOf 水位：nil / 不限 = 0；有限按百分比取整。
func TestQuotaStatusOf(t *testing.T) {
	t.Parallel()
	if got := QuotaStatusOf(nil); got != (QuotaStatus{}) {
		t.Fatalf("nil scope = %+v，want 零值", got)
	}
	sc := quota.NewPool(0).Scope("owner", 200)
	res, err := sc.TryReserve(50)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(50)
	got := QuotaStatusOf(sc)
	if got.Usage != 50 || got.MaxBytes != 200 || got.Watermark != 25 {
		t.Fatalf("got %+v，want usage=50 max=200 watermark=25", got)
	}
}

// TestQuotaStatusOf_Boundaries 水位边界：上限<=0（不限）= 0 不除零；恰好满额 = 100；
// 已用超过上限时**现状不截断**（返回 >100）。
//
// 待决缺陷（本 PR 不改生产代码，仅锁定现状）：`QuotaStatus.Watermark` 字段注释声称
// 「百分比 0-100」，但公开路径（PUT /api/storage/config 热调上限 → Pool.SetMaxBytes）
// 可让上限低于已用 ⇒ 水位 >100（本用例实测 250）。是否截断到 100（会丢失「已超配额」
// 信号）还是把注释改为「可 >100，表示超配」需产品裁定。
func TestQuotaStatusOf_Boundaries(t *testing.T) {
	t.Parallel()
	// 上限 <=0 = 不限 ⇒ 水位 0（不除零）。
	for _, limit := range []int64{0, -1} {
		sc := quota.NewPool(0).Scope("owner", limit)
		if sc == nil {
			t.Fatalf("Scope(limit=%d) 不应为 nil", limit)
		}
		if got := QuotaStatusOf(sc); got.MaxBytes != limit || got.Watermark != 0 {
			t.Fatalf("不限配额 max=%d → %+v，want 原样上限且 watermark=0", limit, got)
		}
	}
	// 恰好满额 ⇒ 100%。
	full := quota.NewPool(0).Scope("owner", 100)
	res, err := full.TryReserve(100)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(100)
	if got := QuotaStatusOf(full); got.Watermark != 100 {
		t.Fatalf("满额 watermark = %d，want 100", got.Watermark)
	}
	// 上限下调到低于已用 ⇒ 现状不截断。
	shrunk := quota.NewPool(0).Scope("owner", 200)
	res, err = shrunk.TryReserve(50)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(50)
	shrunk.Pool().SetMaxBytes(20)
	if got := QuotaStatusOf(shrunk); got.Watermark != 250 || got.Usage != 50 || got.MaxBytes != 20 {
		t.Fatalf("超配额 → %+v，want usage=50 max=20 watermark=250（现状未截断）", got)
	}
}
