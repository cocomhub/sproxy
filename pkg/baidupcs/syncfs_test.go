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
	// fake 存两个文件
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
	// fake List 是递归全量 → 顶层含 a.txt + sub/b.txt（简化：先断言不报错且非空）
	if len(entries) == 0 {
		t.Fatal("ListDir 应非空")
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
