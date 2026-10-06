// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSubFSTestFS 构造一层临时底层 + subFS 看子视图。
func newSubFSTestFS(t *testing.T, subdir string) (*SubFS, string) {
	t.Helper()
	root := t.TempDir()
	base := NewLocalFS(root, nil)
	sub, err := NewSubFS(base, subdir)
	if err != nil {
		t.Fatalf("NewSubFS(%q): %v", subdir, err)
	}
	return sub, root
}

// TestSubFS_WriteConfinedUnderSubdir 可写视图把写落到 <root>/<subdir>/ 下。
func TestSubFS_WriteConfinedUnderSubdir(t *testing.T) {
	t.Parallel()
	sub, baseRoot := newSubFSTestFS(t, "videos")
	ctx := context.Background()
	if err := sub.MakeDir(ctx, "dirA"); err != nil {
		t.Fatalf("MakeDir dirA: %v", err)
	}
	if err := sub.WriteFile(ctx, "dirA/hello.txt", bytes.NewReader([]byte("hi")), 2, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 物理位置在 <root>/videos/dirA/hello.txt
	full := filepath.Join(baseRoot, "videos", "dirA", "hello.txt")
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("读取 <root>/videos/dirA/hello.txt: %v", err)
	}
	if string(data) != "hi" {
		t.Fatalf("内容 = %q, want hi", data)
	}
	// 根目录不应被写（密文不逃出 subdir）
	if _, err := os.Stat(filepath.Join(baseRoot, "dirA")); !os.IsNotExist(err) {
		t.Fatalf("根目录下不应出现 dirA（越界）")
	}
}

// TestSubFS_ReadListStat_RelativeView 读方法正常 + ListDir Path 相对 subdir 根。
func TestSubFS_ReadListStat_RelativeView(t *testing.T) {
	t.Parallel()
	sub, _ := newSubFSTestFS(t, "videos")
	ctx := context.Background()
	if err := sub.WriteFile(ctx, "sub/x.txt", bytes.NewReader([]byte("x")), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e, err := sub.Stat(ctx, "sub/x.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat sub/x.txt = %v/%v, want 存在", e, err)
	}
	entries, err := sub.ListDir(ctx, "sub")
	if err != nil {
		t.Fatalf("ListDir sub: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "x.txt" {
		t.Fatalf("ListDir(sub) = %+v, want 恰含 x.txt", entries)
	}
	if entries[0].Path != "sub/x.txt" {
		t.Fatalf("条目 Path = %q, want 相对 subdir 根 sub/x.txt", entries[0].Path)
	}
	rc, err := sub.OpenRead(ctx, "sub/x.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := readAll(rc)
	rc.Close()
	if string(got) != "x" {
		t.Fatalf("读回 %q, want x", got)
	}
}

// TestSubFS_EscapeRejected 防穿越：`..`/绝对路径被拒，不出 subdir 边界。
func TestSubFS_EscapeRejected(t *testing.T) {
	t.Parallel()
	sub, baseRoot := newSubFSTestFS(t, "videos")
	ctx := context.Background()
	// 逃逸写
	if err := sub.WriteFile(ctx, "../esc.txt", bytes.NewReader([]byte("x")), 1, 0); err == nil {
		t.Fatalf("WriteFile ../esc.txt 应拒绝")
	}
	// 逃逸 MakeDir
	if err := sub.MakeDir(ctx, "/abs"); err == nil {
		t.Fatalf("MakeDir /abs 应拒绝")
	}
	// 底层根不受污染
	if _, err := os.Stat(filepath.Join(baseRoot, "esc.txt")); !os.IsNotExist(err) {
		t.Fatalf("底层层根不应出现 esc.txt")
	}
}

// TestSubFS_EmptySubdirRejected 空子目录非法（需 <卷>/<新子目录>）。
func TestSubFS_EmptySubdirRejected(t *testing.T) {
	t.Parallel()
	base := NewLocalFS(t.TempDir(), nil)
	if _, err := NewSubFS(base, ""); err == nil {
		t.Fatalf("NewSubFS(空子目录) 应报错")
	}
}

// TestReadonlySubFS_WriteFailClosed_ReadPass 只读封装根写保护：
// WriteFile/Rename/Delete/MakeDir fail-closed（ErrReadOnly）；ListDir/Stat/OpenRead 透传。
func TestReadonlySubFS_WriteFailClosed_ReadPass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := NewLocalFS(root, nil)
	// 预写一个文件供读
	if err := base.WriteFile(context.Background(), "videos/a.txt", strings.NewReader("a"), 1, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ro, err := NewReadonlySubFS(base, "videos")
	if err != nil {
		t.Fatalf("NewReadonlySubFS: %v", err)
	}
	ctx := context.Background()
	// 写方法全部 fail-closed
	cases := []struct {
		name string
		do   func() error
	}{
		{"WriteFile", func() error { return ro.WriteFile(ctx, "b.txt", strings.NewReader("x"), 1, 0) }},
		{"MakeDir", func() error { return ro.MakeDir(ctx, "d") }},
		{"Delete", func() error { return ro.Delete(ctx, "a.txt") }},
		{"Rename", func() error { return ro.Rename(ctx, "a.txt", "c.txt") }},
	}
	for _, tc := range cases {
		if lerr := tc.do(); lerr == nil || !strings.Contains(lerr.Error(), "只读") {
			t.Errorf("%s: err = %v, want ErrReadOnly 包装", tc.name, lerr)
		}
	}
	// 读方法透传（Stat/ListDir/OpenRead）
	e, err := ro.Stat(ctx, "a.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat a.txt = %v/%v, want 存在", e, err)
	}
	entries, err := ro.ListDir(ctx, "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListDir = %+v/%v, want 恰含 a.txt", entries, err)
	}
	rc, err := ro.OpenRead(ctx, "a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := readAll(rc)
	rc.Close()
	if string(got) != "a" {
		t.Fatalf("读回 %q, want a", got)
	}
	// 防穿越：读 ../ 越界拒绝
	if e, _ := ro.Stat(ctx, "../x"); e != nil {
		t.Fatalf("只读视图 Stat ../x 越界应返回 nil/err, got %v", e)
	}
}

// readAll 读取 rc 全部（io.ReadAll 便捷）。
func readAll(rc io.ReadCloser) ([]byte, error) {
	if rc == nil {
		return nil, os.ErrClosed
	}
	return io.ReadAll(rc)
}
