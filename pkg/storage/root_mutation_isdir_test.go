// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRootMutation_IsDir 钉住事件携带 IsDir（设计 2026-10-30 D1）：rename/remove 在操作前
// Stat 判定；Mkdir/RemoveAll 恒 true；内容写 false。
//
// 变异：删除赋值（恒 false）⇒ 目录 rename/remove 断言红。
func TestRootMutation_IsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	var got []Mutation
	rt.SetMutationObserver("main", func(m Mutation) { got = append(got, m) })
	last := func() Mutation { return got[len(got)-1] }

	if err := rt.MkdirAll("d", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !last().IsDir || last().Op != MutationMkdir {
		t.Fatalf("Mkdir 事件 IsDir=%v op=%v", last().IsDir, last().Op)
	}
	f, oerr := rt.OpenFile("d/f", os.O_CREATE|os.O_WRONLY, 0o644)
	if oerr != nil {
		t.Fatalf("openfile: %v", oerr)
	}
	if _, werr := f.WriteString("x"); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	_ = f.Close()
	rt.MarkWritten("d/f")
	if last().Op != MutationWrite || last().IsDir {
		t.Fatalf("Write 事件 IsDir=%v op=%v", last().IsDir, last().Op)
	}
	if err := rt.Rename("d/f", "d/g"); err != nil {
		t.Fatalf("rename file: %v", err)
	}
	if last().Op != MutationRename || last().IsDir {
		t.Fatalf("文件 rename IsDir=%v", last().IsDir)
	}
	if err := rt.Rename("d", "e"); err != nil {
		t.Fatalf("rename dir: %v", err)
	}
	if last().Op != MutationRename || !last().IsDir {
		t.Fatalf("目录 rename IsDir=%v", last().IsDir)
	}
	if err := rt.Remove("e/g"); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	if last().Op != MutationRemove || last().IsDir {
		t.Fatalf("文件 remove IsDir=%v", last().IsDir)
	}
	if err := rt.Remove("e"); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if last().Op != MutationRemove || !last().IsDir {
		t.Fatalf("空目录 remove IsDir=%v", last().IsDir)
	}
	if err := rt.MkdirAll(filepath.Join("x", "y"), 0o755); err != nil {
		t.Fatalf("mkdir2: %v", err)
	}
	if err := rt.RemoveAll("x"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	if last().Op != MutationRemoveAll || !last().IsDir {
		t.Fatalf("RemoveAll IsDir=%v", last().IsDir)
	}
}
