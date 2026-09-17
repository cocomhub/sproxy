// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pcsapi "github.com/cocomhub/sproxy/pkg/baidupcs/internal"
)

// fakeStorageAdapter 是 Storage 内部用的 Adapter 的内存 fake（驱动全方法测试）。
type fakeStorageAdapter struct {
	files map[string][]byte // remote path → content
	// 控制 ETag 复核失败次数（模拟 Baidu 分片上传 ETag 语义差异）
	etagMismatchCount int
	uploadCalls       int
}

func newFakeStorageAdapter() *fakeStorageAdapter {
	return &fakeStorageAdapter{files: make(map[string][]byte)}
}

func (f *fakeStorageAdapter) Meta(path string) (*pcsapi.FileDirectory, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, &PCSError{Op: "meta", Category: ErrCategoryNotFound, Err: errors.New("not found")}
	}
	return &pcsapi.FileDirectory{
		Path: path, Filename: filepath.Base(path), Size: int64(len(content)), MD5: md5hex(content),
	}, nil
}

func (f *fakeStorageAdapter) List(path string) ([]*pcsapi.FileDirectory, error) {
	var out []*pcsapi.FileDirectory
	for p, content := range f.files {
		if strings.HasPrefix(p, path+"/") || p == path {
			out = append(out, &pcsapi.FileDirectory{Path: p, Filename: filepath.Base(p), Size: int64(len(content)), MD5: md5hex(content)})
		}
	}
	return out, nil
}

func (f *fakeStorageAdapter) Delete(paths ...string) error {
	for _, p := range paths {
		delete(f.files, p)
	}
	return nil
}

func (f *fakeStorageAdapter) Copy(entries ...*pcsapi.CpMvJSON) error {
	for _, e := range entries {
		if content, ok := f.files[e.From]; ok {
			f.files[e.To] = content
		}
	}
	return nil
}

func (f *fakeStorageAdapter) Move(entries ...*pcsapi.CpMvJSON) error {
	for _, e := range entries {
		if content, ok := f.files[e.From]; ok {
			f.files[e.To] = content
			delete(f.files, e.From)
		}
	}
	return nil
}

func (f *fakeStorageAdapter) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	f.uploadCalls++
	// 模拟 ETag 语义差异：前 N 次上传后 Meta 的 MD5 与本地不符。
	if f.etagMismatchCount > 0 {
		f.files[targetPath] = []byte("tampered-" + string(content))
		f.etagMismatchCount--
		return nil
	}
	f.files[targetPath] = content
	return nil
}

func (f *fakeStorageAdapter) Download(ctx context.Context, remotePath, localPath string) error {
	content, ok := f.files[remotePath]
	if !ok {
		return &PCSError{Op: "get", Category: ErrCategoryNotFound, Err: errors.New("not found")}
	}
	return os.WriteFile(localPath, content, 0o644)
}

func md5hex(b []byte) string {
	return fmt.Sprintf("%x", md5.Sum(b))
}

// storageForTest 构造被测 Storage（root=/baidu，fake adapter）。
func storageForTest(t *testing.T, fake *fakeStorageAdapter) *Storage {
	t.Helper()
	s, err := NewStorage(StorageConfig{Root: "/baidu", Adapter: fake, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewStorage 失败: %v", err)
	}
	return s
}

func TestStorage_PutGetRoundtrip(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	s := storageForTest(t, fake)

	key := "dir/file.txt"
	body := []byte("hello world")
	if _, err := s.Put(context.Background(), key, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put 失败: %v", err)
	}

	rc, meta, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("Get 内容 = %q, want %q", got, body)
	}
	if meta == nil || meta.Key != key {
		t.Fatalf("meta = %+v, want key %q", meta, key)
	}
}

func TestStorage_Stat_NotFound(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	s := storageForTest(t, fake)

	if _, err := s.Stat(context.Background(), "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat 缺失应 ErrNotFound, got %v", err)
	}
}

func TestStorage_List_Recursive(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	// 预置多个文件（直接写 fake）
	fake.files["/baidu/a.txt"] = []byte("a")
	fake.files["/baidu/dir/b.txt"] = []byte("b")
	fake.files["/baidu/dir/sub/c.txt"] = []byte("c")
	s := storageForTest(t, fake)

	items, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("List 应 3 项, got %d (%v)", len(items), items)
	}
}

func TestStorage_Delete_Missing(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	s := storageForTest(t, fake)

	// 删除缺失文件：fake Delete 幂等（不存在视为成功——与网盘语义一致）。
	if err := s.Delete(context.Background(), "nope.txt"); err != nil {
		t.Fatalf("Delete 缺失应成功(幂等), got %v", err)
	}
}

func TestStorage_Put_ETagMismatch_Retries(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	fake.etagMismatchCount = 2 // 前 2 次上传后 ETag 复核失败，第 3 次成功
	s := storageForTest(t, fake)

	body := []byte("etag-test")
	if _, err := s.Put(context.Background(), "f.txt", bytes.NewReader(body)); err != nil {
		t.Fatalf("Put 应有界重试后成功, got %v", err)
	}
	if fake.uploadCalls != 3 {
		t.Fatalf("uploadCalls = %d, want 3（有界重试）", fake.uploadCalls)
	}
}

func TestStorage_Put_ETagMismatch_ExceedsRetries(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	fake.etagMismatchCount = 5 // 一直失败（超过上限 3）
	s := storageForTest(t, fake)

	body := []byte("etag-fail")
	if _, err := s.Put(context.Background(), "f.txt", bytes.NewReader(body)); err == nil {
		t.Fatal("Put 应因 ETag 复核持续失败而报错")
	}
	if fake.uploadCalls != maxPutAttempts {
		t.Fatalf("uploadCalls = %d, want %d（上限内停止）", fake.uploadCalls, maxPutAttempts)
	}
}

func TestStorage_CopyMove(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	fake.files["/baidu/src.txt"] = []byte("src")
	s := storageForTest(t, fake)

	if _, err := s.Copy(context.Background(), "src.txt", "dst.txt"); err != nil {
		t.Fatalf("Copy 失败: %v", err)
	}
	if _, err := s.Move(context.Background(), "src.txt", "moved.txt"); err != nil {
		t.Fatalf("Move 失败: %v", err)
	}
	// src 已不存在，moved 存在。
	if _, err := s.Stat(context.Background(), "src.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Move 后 src 应不存在, got %v", err)
	}
	if _, err := s.Stat(context.Background(), "moved.txt"); err != nil {
		t.Fatalf("Move 后 moved 应存在: %v", err)
	}
}

func TestStorage_RemotePath_RejectsTraversal(t *testing.T) {
	t.Parallel()
	fake := newFakeStorageAdapter()
	s := storageForTest(t, fake)

	if _, err := s.Put(context.Background(), "../evil.txt", bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
}
