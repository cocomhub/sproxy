// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

// federated_test.go 验证联邦卷只读适配（roadmap 3.3 P2 F1 片）：
//  1. 读方法（ListDir/Stat/OpenRead）转发注入的 Reader。
//  2. 写方法（WriteFile/Rename/Delete/MakeDir）恒 ErrReadOnly（fail-closed）。
//  3. nil Reader 构造拒绝（fail-fast）。

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// mockReader 是注入的只读实现（记录调用 + 返回固定结果）。
type mockReader struct {
	entries map[string][]syncpkg.Entry
	files   map[string]string
}

func (m *mockReader) ListDir(_ context.Context, path string) ([]syncpkg.Entry, error) {
	return m.entries[path], nil
}
func (m *mockReader) Stat(_ context.Context, path string) (*syncpkg.Entry, error) {
	if _, ok := m.files[path]; ok {
		return &syncpkg.Entry{Name: path, Size: int64(len(m.files[path]))}, nil
	}
	return nil, os.ErrNotExist
}
func (m *mockReader) OpenRead(_ context.Context, path string) (io.ReadCloser, error) {
	if _, ok := m.files[path]; !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(m.files[path])), nil
}

// TestFS_ReadForwarding 读方法转发注入 Reader。
func TestFS_ReadForwarding(t *testing.T) {
	t.Parallel()
	r := &mockReader{
		entries: map[string][]syncpkg.Entry{
			"": {{Name: "a.txt", Size: 5}},
		},
		files: map[string]string{"a.txt": "hello"},
	}
	fs, err := New(r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	entries, err := fs.ListDir(ctx, "")
	if err != nil || len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("ListDir = %+v err=%v, want [a.txt]", entries, err)
	}
	st, err := fs.Stat(ctx, "a.txt")
	if err != nil || st.Size != 5 {
		t.Fatalf("Stat = %+v err=%v, want size 5", st, err)
	}
	rc, err := fs.OpenRead(ctx, "a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Fatalf("OpenRead 内容 = %q, want hello", data)
	}
}

// TestFS_WriteRejected 写方法恒 ErrReadOnly。
func TestFS_WriteRejected(t *testing.T) {
	t.Parallel()
	fs, err := New(&mockReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "x", strings.NewReader(""), 0, 0); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteFile err = %v, want ErrReadOnly", err)
	}
	if err := fs.Rename(ctx, "a", "b"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Rename err = %v, want ErrReadOnly", err)
	}
	if err := fs.Delete(ctx, "a"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Delete err = %v, want ErrReadOnly", err)
	}
	if err := fs.MakeDir(ctx, "d"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("MakeDir err = %v, want ErrReadOnly", err)
	}
}

// TestFS_NilReaderRejected nil Reader 构造拒绝。
func TestFS_NilReaderRejected(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatal("nil Reader 应拒绝")
	}
}
