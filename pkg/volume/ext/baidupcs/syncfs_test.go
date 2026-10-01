// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newTestStorageFS 构造 StorageFS（fake Storage + 临时本地布局）。
func newTestStorageFS(t *testing.T) *StorageFS {
	t.Helper()
	return &StorageFS{
		s:    newFakeStorage(),
		temp: t.TempDir(),
	}
}

func TestStorageFS_ListDir(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	// fake 存两个文件 + 一个子目录
	if _, err := fs.s.Put(context.Background(), "a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(context.Background(), "sub/b.txt", strings.NewReader("world")); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ListDir(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// 单层：顶层含 a.txt（文件）+ sub（目录），不含 sub/b.txt
	byPath := make(map[string]syncpkg.Entry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath["a.txt"]; !ok {
		t.Fatalf("顶层缺 a.txt，got paths=%v", pathsOf(entries))
	}
	sub, ok := byPath["sub"]
	if !ok {
		t.Fatalf("顶层缺 sub 目录，got paths=%v", pathsOf(entries))
	}
	if !sub.IsDir {
		t.Fatalf("sub 应为目录，got %+v", sub)
	}
	if _, ok := byPath["sub/b.txt"]; ok {
		t.Fatalf("ListDir 不应递归（出现 sub/b.txt）")
	}
}

func pathsOf(entries []syncpkg.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

// TestStorageFS_ListDir_TreeWalk 验证 sync 引擎 WalkEntries 对 StorageFS 递归遍历树。
func TestStorageFS_ListDir_TreeWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := newTestStorageFS(t)
	if _, err := fs.s.Put(ctx, "a.txt", strings.NewReader("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(ctx, "sub/b.txt", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.s.Put(ctx, "sub/deep/c.txt", strings.NewReader("c")); err != nil {
		t.Fatal(err)
	}
	entries, err := syncpkg.WalkEntries(ctx, fs, "", true, false, nil)
	if err != nil {
		t.Fatalf("WalkEntries: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Path] = true
	}
	// WalkEntries 只对空目录输出目录条目：'sub'、'sub/deep' 均非空（含文件）→ 以子文件体现。
	for _, want := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
		if !got[want] {
			t.Fatalf("WalkEntries 缺 %q，got=%v", want, got)
		}
	}
}

func TestStorageFS_Stat_Missing(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	e, err := fs.Stat(context.Background(), "nope.txt")
	if err != nil {
		t.Fatalf("Stat 不存在应返回 (nil,nil)，got err=%v", err)
	}
	if e != nil {
		t.Fatalf("Stat 不存在应返回 nil，got %+v", e)
	}
}

func TestStorageFS_WriteRead(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), "f.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	rc, err := fs.OpenRead(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("ReadAll = %q, want data", got)
	}
}

func TestStorageFS_Delete(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), "f.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(context.Background(), "f.txt"); err != nil {
		t.Fatal(err)
	}
	e, err := fs.Stat(context.Background(), "f.txt")
	if err != nil || e != nil {
		t.Fatalf("Delete 后 Stat 应 nil，got e=%+v err=%v", e, err)
	}
}

func TestStorageFS_Rename(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	if err := fs.WriteFile(context.Background(), "from.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	if err := fs.Rename(context.Background(), "from.txt", "to.txt"); err != nil {
		t.Fatal(err)
	}
	// from 不存在、to 存在
	e, err := fs.Stat(context.Background(), "from.txt")
	if err != nil || e != nil {
		t.Fatalf("Rename 后 from 应 nil，got e=%+v err=%v", e, err)
	}
	rc, err := fs.OpenRead(context.Background(), "to.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "data" {
		t.Fatalf("to.txt = %q, want data", got)
	}
}

var _ syncpkg.FS = (*StorageFS)(nil)
