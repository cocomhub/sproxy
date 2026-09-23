// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

// write_test.go 验证联邦卷回写（roadmap P2 联邦卷回写）：
//  1. 未注入 Writer → 写方法恒 ErrReadOnly（零回归 fail-closed）。
//  2. WithWriter 注入写面 → WriteFile/Rename/Delete 转发成功。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memWriter 内存写面 sync.FS（测试）。
type memWriter struct {
	files map[string]string
}

func (m *memWriter) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	b, _ := io.ReadAll(r)
	m.files[path] = string(b)
	return nil
}
func (m *memWriter) Rename(ctx context.Context, from, to string) error {
	m.files[to] = m.files[from]
	delete(m.files, from)
	return nil
}
func (m *memWriter) Delete(ctx context.Context, path string) error {
	delete(m.files, path)
	return nil
}
func (m *memWriter) MakeDir(ctx context.Context, path string) error { return nil }
func (m *memWriter) OpenWriterAt(ctx context.Context, path string, size, mtime int64) (io.WriterAt, io.Closer, error) {
	return nil, nil, errors.New("not impl")
}
func (m *memWriter) OpenReaderAt(ctx context.Context, path string) (io.ReaderAt, io.Closer, error) {
	return nil, nil, errors.New("not impl")
}
func (m *memWriter) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return nil, nil
}
func (m *memWriter) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) { return nil, nil }
func (m *memWriter) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return nil, errors.New("not impl")
}

// 编译期断言：memWriter 实现 sync.FS。
var _ syncpkg.FS = (*memWriter)(nil)

// TestFS_WithoutWriter_ErrReadOnly 未注入 Writer → 写方法恒 ErrReadOnly。
func TestFS_WithoutWriter_ErrReadOnly(t *testing.T) {
	t.Parallel()
	fs, err := New(&memWriter{files: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 0); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteFile = %v, want ErrReadOnly", err)
	}
	if err := fs.Rename(context.Background(), "a", "b"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Rename = %v, want ErrReadOnly", err)
	}
	if err := fs.Delete(context.Background(), "a"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Delete = %v, want ErrReadOnly", err)
	}
}

// TestFS_WithWriter_WriteForwards 注入 Writer → 写方法转发成功。
func TestFS_WithWriter_WriteForwards(t *testing.T) {
	t.Parallel()
	w := &memWriter{files: map[string]string{}}
	fs, err := New(&memWriter{files: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	fs.WithWriter(w)
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("hello"), 5, 0); err != nil {
		t.Fatalf("WriteFile = %v", err)
	}
	if w.files["a.txt"] != "hello" {
		t.Fatalf("写入 = %q", w.files["a.txt"])
	}
	if err := fs.Rename(context.Background(), "a.txt", "b.txt"); err != nil {
		t.Fatalf("Rename = %v", err)
	}
	if _, ok := w.files["b.txt"]; !ok {
		t.Fatal("Rename 后 b.txt 应存在")
	}
	if err := fs.Delete(context.Background(), "b.txt"); err != nil {
		t.Fatalf("Delete = %v", err)
	}
	if _, ok := w.files["b.txt"]; ok {
		t.Fatal("Delete 后 b.txt 应不存在")
	}
}
