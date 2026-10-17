// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newRootFSFixture 建一个含 `x.bin` 与 `d/f.bin` 的卷根适配器。
func newRootFSFixture(t *testing.T) (syncpkg.FS, *storage.Root) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"x.bin", "d/f.bin"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f)), []byte("payload-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return NewRootFS(rt), rt
}

// TestRootFS_EmptyRelIsVolumeRoot `sync.FS` 契约里**空串 = 卷根**（`LocalFS` 的
// `SanitizeRelPath("")` 返回 ""，`sync.WalkEntries(ctx, fs, "", …)`/`backup.Run` 均以 "" 起步）；
// 但本适配器此前把 "" 直接交给底座 `os.Root`，而 `os.Root.Open("")`/`Stat("")` 以
// `openat : empty path` 失败 ⇒ 「列/stat 卷根」恒失败 = 任何从 "" 起步的遍历（备份、sync 树扫描、
// WebDAV 远端视图）在本适配器上不可用。
//
// 变异：去掉 `rootRel` 映射 → ListDir/Stat/WalkEntries("") 全部报 `empty path` → 红。
func TestRootFS_EmptyRelIsVolumeRoot(t *testing.T) {
	t.Parallel()
	fsys, _ := newRootFSFixture(t)

	ents, err := fsys.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir(\"\") 应返回卷根条目: %v", err)
	}
	// 子条目 Path 必须是**裸相对路径**（无 "." 前缀），与 LocalFS 一致。
	seen := map[string]bool{}
	for _, e := range ents {
		if strings.HasPrefix(e.Path, ".") || strings.Contains(e.Path, "./") {
			t.Fatalf("条目 Path 应为裸相对路径（不得带 . 前缀）, got %q", e.Path)
		}
		seen[e.Path] = true
	}
	if !seen["x.bin"] || !seen["d"] {
		t.Fatalf("卷根列举应含 fixture 条目, got %v", seen)
	}
	st, err := fsys.Stat(context.Background(), "")
	if err != nil || st == nil || !st.IsDir {
		t.Fatalf("Stat(\"\") 应为目录: %+v err=%v", st, err)
	}
	// 集成点：从 "" 起步的整树遍历（备份/sync 的实现方式）。
	walked, werr := syncpkg.WalkEntries(context.Background(), fsys, "", true, false, nil)
	if werr != nil {
		t.Fatalf("WalkEntries(\"\") 应可用（备份/sync 的实现方式）: %v", werr)
	}
	if len(walked) < 3 {
		t.Fatalf("整树遍历应含根/目录/文件, got %d: %+v", len(walked), walked)
	}
}

// TestRootFS_WriteReadDeleteAtRootKey 卷根适配器的写/读/删在空串与正常路径下语义一致。
func TestRootFS_WriteReadDeleteAtRootKey(t *testing.T) {
	t.Parallel()
	fsys, _ := newRootFSFixture(t)
	ctx := context.Background()
	if err := fsys.WriteFile(ctx, syncpkg.LogicalKey{}, "sub/new.bin", bytes.NewReader([]byte("hello")), 5, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fsys.OpenRead(ctx, "sub/new.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("读回=%q", got)
	}
	if err := fsys.Delete(ctx, "sub/new.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if st, serr := fsys.Stat(ctx, "sub/new.bin"); serr != nil || st != nil {
		t.Fatalf("删除后应不存在: %+v err=%v", st, serr)
	}
}
