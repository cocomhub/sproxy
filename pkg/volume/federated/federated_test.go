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

// rangeReader 是带 OpenRangeRead 的 mockReader（测试 RangeReader 透传）。
type rangeReader struct {
	mockReader
}

func (r *rangeReader) OpenRangeRead(_ context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	if _, ok := r.files[path]; !ok {
		return nil, os.ErrNotExist
	}
	data := r.files[path]
	if offset < 0 || offset+size > int64(len(data)) {
		return nil, os.ErrInvalid
	}
	return io.NopCloser(strings.NewReader(data[offset : offset+size])), nil
}

// TestFS_OpenRangeRead_Passthrough（集群出口地基回归 2026-10-05）：底层 Reader 实现
// RangeReader 时透传；无 RangeReader 的 Reader → 报错（fail-closed）。
func TestFS_OpenRangeRead_Passthrough(t *testing.T) {
	t.Parallel()
	// 1) 底层带 RangeReader → 透传区间读。
	rr := &rangeReader{mockReader: mockReader{files: map[string]string{"docs/a.bin": "0123456789"}}}
	fs, err := New(rr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rc, rerr := fs.OpenRangeRead(context.Background(), "docs/a.bin", 2, 5)
	if rerr != nil {
		t.Fatalf("OpenRangeRead: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	rc.Close()
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	if string(got) != "23456" {
		t.Fatalf("Range 段=%q want 23456", got)
	}
	// 2) 底层无 RangeReader → 报错（fail-closed）。
	plain := &mockReader{files: map[string]string{"x": "y"}}
	fs2, _ := New(plain)
	if _, err := fs2.OpenRangeRead(context.Background(), "x", 0, 1); err == nil {
		t.Fatal("无 RangeReader 底层应报错")
	}
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
