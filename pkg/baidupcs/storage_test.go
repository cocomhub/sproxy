// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeStorageAdapter 是 Storage 测试用的内存 adapter（实现 Adapter 接口）。
type fakeStorageAdapter struct {
	files map[string]string // remote path → content
}

func newFakeStorageAdapter() *fakeStorageAdapter {
	return &fakeStorageAdapter{files: make(map[string]string)}
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
	return nil
}

func (f *fakeStorageAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	data, ok := f.files[remotePath]
	if !ok {
		return errNotFound
	}
	return os.WriteFile(localPath, []byte(data), 0o644)
}

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
	entries, err := os.ReadDir(s.temp)
	if err != nil {
		t.Fatal(err)
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
