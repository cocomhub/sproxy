// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeStorageAdapter 是 Storage 测试用的内存 adapter（实现 Adapter 接口）。
type fakeStorageAdapter struct {
	files map[string]string   // remote path → content
	dirs  map[string]struct{} // 目录标记（remote path → exists）
}

func newFakeStorageAdapter() *fakeStorageAdapter {
	return &fakeStorageAdapter{
		files: make(map[string]string),
		dirs:  make(map[string]struct{}),
	}
}

func (f *fakeStorageAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	if _, exists := f.files[targetPath]; exists && !overwrite {
		return errAlreadyExists
	}
	f.files[targetPath] = string(data)
	// 父目录标记（目录语义：dir/ 前缀的路径隐式建目录）。
	f.markDirs(targetPath)
	return nil
}

func (f *fakeStorageAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	data, ok := f.files[remotePath]
	if !ok {
		return errNotFound
	}
	return os.WriteFile(localPath, []byte(data), 0o644)
}

// markDirs 为 remotePath 的所有父路径建目录标记。
func (f *fakeStorageAdapter) markDirs(remotePath string) {
	dir := path.Dir(strings.TrimSuffix(remotePath, "/"))
	for dir != "/" && dir != "." && dir != "" {
		f.dirs[dir] = struct{}{}
		dir = path.Dir(dir)
	}
	f.dirs["/"] = struct{}{}
}

// List 返回 remotePath 下单层条目（目录+文件混合，不递归）。实现 metadataProvider。
// 条目 Key 为**完整 remote 路径**（调用方 Storage.List 剥 root 得相对路径）。
func (f *fakeStorageAdapter) List(ctx context.Context, remotePath string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, isDir := f.dirs[remotePath]; !isDir {
		if _, isFile := f.files[remotePath]; isFile {
			// 单文件路径：返回自身
			return []ObjectMeta{{Key: remotePath, Size: int64(len(f.files[remotePath]))}}, nil
		}
		return nil, errNotFound
	}
	base := strings.TrimSuffix(remotePath, "/")
	if base == "/" {
		base = ""
	}
	prefix := base
	if prefix != "" {
		prefix += "/"
	}
	out := make([]ObjectMeta, 0)
	seen := map[string]bool{}
	// 文件（单层：prefix 下的直接子项，key 保持完整 remote 路径）
	for k := range f.files {
		rel, ok := strings.CutPrefix(k, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: k, Size: int64(len(f.files[k]))})
	}
	// 目录（单层子目录，key 保持完整 remote 路径）
	for d := range f.dirs {
		if d == "/" || d == remotePath {
			continue
		}
		rel, ok := strings.CutPrefix(d, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, ObjectMeta{Key: d, IsDir: true})
	}
	return out, nil
}

// Meta 返回单个路径元信息（目录/文件）。实现 metadataProvider。
func (f *fakeStorageAdapter) Meta(ctx context.Context, remotePath string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, isDir := f.dirs[remotePath]; isDir {
		return &ObjectMeta{Key: remotePath, IsDir: true}, nil
	}
	data, ok := f.files[remotePath]
	if !ok {
		return nil, errNotFound
	}
	return &ObjectMeta{Key: remotePath, Size: int64(len(data)), ModTime: time.Now()}, nil
}

var _ metadataProvider = (*fakeStorageAdapter)(nil)

// errAlreadyExists / errNotFound 是 fake 内部哨兵（storage 层映射为公开错误）。
var (
	errAlreadyExists = errors.New("already exists")
	errNotFound      = errors.New("not found")
)

func TestStorage_PutGetRoundtrip(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	meta, err := s.Put(context.Background(), "dir/f.txt", strings.NewReader("hello baidu"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Size != int64(len("hello baidu")) {
		t.Fatalf("meta.Size = %d, want %d", meta.Size, len("hello baidu"))
	}
	rc, m2, err := s.Get(context.Background(), "dir/f.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello baidu" {
		t.Fatalf("Get 内容 = %q, want %q", got, "hello baidu")
	}
	if m2.Key != "dir/f.txt" {
		t.Fatalf("meta.Key = %q, want dir/f.txt", m2.Key)
	}
}

func TestStorage_Stat_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if _, err := s.Stat(context.Background(), "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat 缺失应 ErrNotFound, got %v", err)
	}
}

func TestStorage_Put_StatNotFound_Retries(t *testing.T) {
	t.Parallel()
	// fake adapter 上传后 Stat 恒报 NotFound（百度最终一致性）→ 有界重试 ≤3 后 ErrTransient
	ad := &statMissingAdapter{inner: newFakeStorageAdapter()}
	s := newTestStorage(t, ad)
	_, err := s.Put(context.Background(), "f.txt", strings.NewReader("data"))
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("Stat 恒 NotFound 应 ErrTransient, got %v", err)
	}
	if ad.uploads > maxPutAttempts {
		t.Fatalf("重试次数 %d 超过上限 %d", ad.uploads, maxPutAttempts)
	}
	if ad.uploads != maxPutAttempts {
		t.Fatalf("应重试 %d 次, got %d", maxPutAttempts, ad.uploads)
	}
}

func TestStorage_Get_TempCleanup(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "f.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Get(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 临时文件应被清理（temp 目录无残留）
	entries, readDirErr := os.ReadDir(s.temp)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("Get 后临时文件未清理: %s", e.Name())
		}
	}
}

func TestStorage_Delete_Missing(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if err := s.Delete(context.Background(), "nope.txt"); err != nil {
		t.Fatalf("删除缺失文件不应报错（幂等）: %v", err)
	}
}

func TestStorage_List_Recursive(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	// fake 只支持精确路径（无目录遍历）——List 依赖 adapter 的目录语义，
	// 本测试用单文件断言（List 对文件路径返回单元素）。
	if _, err := s.Put(context.Background(), "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 || metas[0].Key != "a.txt" {
		t.Fatalf("List 文件 = %+v, want [a.txt]", metas)
	}
}

// statMissingAdapter 上传后 Stat（Download）恒报 NotFound（触发有界重试）。
type statMissingAdapter struct {
	inner   *fakeStorageAdapter
	uploads int
}

func (e *statMissingAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	e.uploads++
	return e.inner.Upload(ctx, localPath, targetPath, overwrite)
}

func (e *statMissingAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	return errNotFound
}

// newTestStorage 构造测试用 Storage（temp 指向 t.TempDir()）。
func newTestStorage(t *testing.T, ad Adapter) *Storage {
	t.Helper()
	s, err := NewStorage(StorageConfig{
		Root:    "/baidu",
		TempDir: t.TempDir(),
		Adapter: ad,
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	if s.temp == "" {
		t.Fatal("s.temp 不应为空")
	}
	return s
}

var _ = filepath.Join // 保留 filepath 导入（后续用例可能用）

// metadataProvider 断言测试辅助：fakeStorageAdapter 需实现 List/Meta（单层目录语义）。

// TestStorage_List_ReturnsChildren 验证 List(prefix) 返回 prefix 下单层条目（目录+文件混合）。
func TestStorage_List_ReturnsChildren(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	// fake adapter 预置：目录 dir/（含 a.txt/b.txt）+ 顶层 c.txt
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "dir/b.txt", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "c.txt", strings.NewReader("c")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byKey := make(map[string]ObjectMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	if _, ok := byKey["dir"]; !ok {
		t.Fatalf("顶层缺 dir 目录条目，got keys=%v", keysOf(metas))
	}
	if !byKey["dir"].IsDir {
		t.Fatalf("dir 应为目录（IsDir=true），got %+v", byKey["dir"])
	}
	if _, ok := byKey["c.txt"]; !ok {
		t.Fatalf("顶层缺 c.txt，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/a.txt"]; ok {
		t.Fatalf("List 不应递归子目录（出现 dir/a.txt）")
	}
}

func keysOf(metas []ObjectMeta) []string {
	out := make([]string, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.Key)
	}
	return out
}

// TestStorage_List_UnderSubdir 验证 List(subdir) 只列该子目录单层。
func TestStorage_List_UnderSubdir(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), "dir/sub/x.txt", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	metas, err := s.List(context.Background(), "dir")
	if err != nil {
		t.Fatalf("List(dir): %v", err)
	}
	byKey := make(map[string]ObjectMeta, len(metas))
	for _, m := range metas {
		byKey[m.Key] = m
	}
	if _, ok := byKey["dir/a.txt"]; !ok {
		t.Fatalf("dir 下缺 dir/a.txt，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/sub"]; !ok || !byKey["dir/sub"].IsDir {
		t.Fatalf("dir 下缺 dir/sub 目录条目，got keys=%v", keysOf(metas))
	}
	if _, ok := byKey["dir/sub/x.txt"]; ok {
		t.Fatalf("List 不应递归子目录（出现 dir/sub/x.txt）")
	}
}

// TestStorage_Stat_UsesMeta 验证 Stat 的 isdir/size 来自库 Meta（不经临时下载）。
func TestStorage_Stat_UsesMeta(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)
	if _, err := s.Put(context.Background(), "dir/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	dirMeta, err := s.Stat(context.Background(), "dir")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if !dirMeta.IsDir {
		t.Fatalf("dir 应 IsDir=true，got %+v", dirMeta)
	}
	fileMeta, err := s.Stat(context.Background(), "dir/a.txt")
	if err != nil {
		t.Fatalf("Stat(a.txt): %v", err)
	}
	if fileMeta.IsDir {
		t.Fatalf("a.txt 不应 IsDir")
	}
	if fileMeta.Size != int64(len("hello")) {
		t.Fatalf("a.txt size = %d, want %d", fileMeta.Size, len("hello"))
	}
}
