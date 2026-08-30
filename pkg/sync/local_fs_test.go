// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testLocalFS(t *testing.T) (*LocalFS, string) {
	t.Helper()
	root := t.TempDir()
	return NewLocalFS(root, nil), root
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestLocalFS_ListDir(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := l.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("根目录应有 2 个条目，got %d (%+v)", len(entries), entries)
	}
	var file *Entry
	var dir *Entry
	for i := range entries {
		switch entries[i].Name {
		case "a.txt":
			file = &entries[i]
		case "sub":
			dir = &entries[i]
		}
	}
	if file == nil {
		t.Fatalf("缺少 a.txt 条目")
	}
	if file.Size != 5 || file.Checksum != sha256Hex([]byte("hello")) || file.IsDir {
		t.Fatalf("a.txt 条目不符: %+v", file)
	}
	if dir == nil || !dir.IsDir || dir.Checksum != "" {
		t.Fatalf("sub 应为目录且 checksum 为空: %+v", dir)
	}
}

func TestLocalFS_ListDir_Subdir(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := l.ListDir(context.Background(), "sub")
	if err != nil {
		t.Fatalf("ListDir error: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "b.txt" {
		t.Fatalf("sub 目录条目不符: %+v", entries)
	}
	if entries[0].Path != "sub/b.txt" {
		t.Fatalf("子目录条目 Path 应为 sub/b.txt，got %q", entries[0].Path)
	}
}

func TestLocalFS_Stat(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := l.Stat(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if e == nil {
		t.Fatalf("Stat 应返回条目")
	}
	if e.Size != 5 || e.Checksum != sha256Hex([]byte("hello")) || e.IsDir {
		t.Fatalf("文件 Stat 不符: %+v", e)
	}

	de, err := l.Stat(context.Background(), "subdir")
	if err != nil {
		t.Fatalf("缺失路径 Stat 不应 error: %v", err)
	}
	if de != nil {
		t.Fatalf("缺失路径应返回 (nil, nil)，got %+v", de)
	}
}

func TestLocalFS_Stat_Dir(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := l.Stat(context.Background(), "d")
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if e == nil || !e.IsDir || e.Checksum != "" {
		t.Fatalf("目录 Stat 不符: %+v", e)
	}
}

func TestLocalFS_OpenRead(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := l.OpenRead(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("OpenRead error: %v", err)
	}
	defer rc.Close()
	data, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(data) {
		t.Fatalf("OpenRead 内容不符: got %q want %q", buf.String(), data)
	}
}

func TestLocalFS_WriteFile(t *testing.T) {
	l, _ := testLocalFS(t)
	mtime := time.Unix(1700000000, 0).UnixNano()
	err := l.WriteFile(context.Background(), "sub/file.txt", strings.NewReader("hello"), 5, mtime)
	if err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	// 验证内容
	e, err := l.Stat(context.Background(), "sub/file.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat after write error: %v, %+v", err, e)
	}
	if e.Size != 5 || e.Checksum != sha256Hex([]byte("hello")) {
		t.Fatalf("写入后条目不符: %+v", e)
	}
	if e.MTime != mtime {
		t.Fatalf("mtime 未保留: got %d want %d", e.MTime, mtime)
	}
}

func TestLocalFS_WriteFile_EmptyFile(t *testing.T) {
	l, _ := testLocalFS(t)
	err := l.WriteFile(context.Background(), "empty.txt", strings.NewReader(""), 0, 123)
	if err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	e, err := l.Stat(context.Background(), "empty.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat after empty write error: %v", err)
	}
	if e.Size != 0 {
		t.Fatalf("空文件 Size 应为 0，got %d", e.Size)
	}
}

func TestLocalFS_Rename(t *testing.T) {
	l, _ := testLocalFS(t)
	if err := l.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Rename(context.Background(), "a.txt", "b.txt"); err != nil {
		t.Fatalf("Rename error: %v", err)
	}
	if e, _ := l.Stat(context.Background(), "a.txt"); e != nil {
		t.Fatalf("rename 后旧路径应不存在")
	}
	if e, _ := l.Stat(context.Background(), "b.txt"); e == nil {
		t.Fatalf("rename 后新路径应存在")
	}
}

func TestLocalFS_Delete(t *testing.T) {
	l, _ := testLocalFS(t)
	if err := l.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if e, _ := l.Stat(context.Background(), "a.txt"); e != nil {
		t.Fatalf("delete 后路径应不存在")
	}
}

func TestLocalFS_MakeDir(t *testing.T) {
	l, root := testLocalFS(t)
	if err := l.MakeDir(context.Background(), "x/y"); err != nil {
		t.Fatalf("MakeDir error: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "x", "y")); err != nil || !info.IsDir() {
		t.Fatalf("MakeDir 未创建目录: %v", err)
	}
}

// TestLocalFS_PathTraversal 验证路径穿越/绝对路径/空字节被拒绝。
func TestLocalFS_PathTraversal(t *testing.T) {
	l, _ := testLocalFS(t)
	ctx := context.Background()
	badPaths := []string{
		"..",
		"../escape",
		"a/../../escape",
		"/abs",
		"a\x00b",
	}
	if runtime.GOOS == "windows" {
		badPaths = append(badPaths, `\abs`, `..\escape`)
	}
	ops := map[string]func(string) error{
		"Stat":    func(p string) error { _, err := l.Stat(ctx, p); return err },
		"ListDir": func(p string) error { _, err := l.ListDir(ctx, p); return err },
		"OpenRead": func(p string) error {
			rc, err := l.OpenRead(ctx, p)
			if err == nil {
				rc.Close()
			}
			return err
		},
		"WriteFile": func(p string) error { return l.WriteFile(ctx, p, strings.NewReader("x"), 1, 1) },
		"Rename":    func(p string) error { return l.Rename(ctx, p, "ok.txt") },
		"Delete":    func(p string) error { return l.Delete(ctx, p) },
		"MakeDir":   func(p string) error { return l.MakeDir(ctx, p) },
	}
	for _, p := range badPaths {
		for op, fn := range ops {
			if err := fn(p); err == nil {
				t.Fatalf("路径 %q 在操作 %s 应被拒绝，但未报错", p, op)
			}
		}
	}
}

// TestLocalFS_WindowsSeparator 验证正斜杠路径在 Windows 落地为反斜杠，且反斜杠输入被归一。
func TestLocalFS_WindowsSeparator(t *testing.T) {
	l, root := testLocalFS(t)
	ctx := context.Background()
	// 正斜杠 relPath → 落地为 Windows 原生反斜杠
	if err := l.WriteFile(ctx, "sub/file.txt", strings.NewReader("x"), 1, 1); err != nil {
		t.Fatalf("WriteFile(正斜杠) error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "file.txt")); err != nil {
		t.Fatalf("正斜杠路径未正确落地: %v", err)
	}
	// Stat 返回正斜杠 Path
	e, err := l.Stat(ctx, "sub/file.txt")
	if err != nil || e == nil || e.Path != "sub/file.txt" {
		t.Fatalf("Stat 返回的 Path 应为正斜杠，got %+v err=%v", e, err)
	}
	if runtime.GOOS == "windows" {
		// 反斜杠输入在 Windows 上被归一为正斜杠处理
		if err := l.WriteFile(ctx, `win\file.txt`, strings.NewReader("y"), 1, 1); err != nil {
			t.Fatalf("WriteFile(反斜杠) error: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "win", "file.txt")); err != nil {
			t.Fatalf("反斜杠路径未在 Windows 归一: %v", err)
		}
	}
}

// TestLocalFS_SymlinkDetection 验证 ListDir 用 Lstat 判定符号链接。
// Windows 无特权时 os.Symlink 失败则跳过。
func TestLocalFS_SymlinkDetection(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "link.txt")
	if err := os.Symlink("target.txt", linkPath); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}
	entries, err := l.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir error: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "link.txt" {
			found = true
			if !e.IsSymlink {
				t.Fatalf("符号链接条目 IsSymlink 应为 true，got %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("缺少符号链接条目")
	}
}

// TestLocalFS_PathTraversal_DriveLetter 验证 Windows 盘符路径被拒绝（审查 M2）。
func TestLocalFS_PathTraversal_DriveLetter(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("盘符路径仅 Windows 语义")
	}
	l, _ := testLocalFS(t)
	ctx := context.Background()
	for _, p := range []string{`C:\foo`, `C:foo`, `c:/foo`, `C:\foo\bar`} {
		if _, err := l.Stat(ctx, p); err == nil {
			t.Fatalf("盘符路径 %q 应被拒绝，但未报错", p)
		}
	}
}

// TestLocalFS_PathTraversal_CleanDot 验证 path.Clean 归一到 "." 的输入被拒绝（审查 M3）。
func TestLocalFS_PathTraversal_CleanDot(t *testing.T) {
	l, _ := testLocalFS(t)
	ctx := context.Background()
	for _, p := range []string{"a/..", "a/./..", "./"} {
		if _, err := l.Stat(ctx, p); err == nil {
			t.Fatalf("路径 %q 归一到 . 应被拒绝，但未报错", p)
		}
	}
}

// TestLocalFS_CtxCancelled 验证 ctx 已取消时所有操作快速失败（审查 I-3）。
func TestLocalFS_CtxCancelled(t *testing.T) {
	l, root := testLocalFS(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ops := map[string]func() error{
		"Stat":     func() error { _, err := l.Stat(ctx, "a.txt"); return err },
		"ListDir":  func() error { _, err := l.ListDir(ctx, ""); return err },
		"OpenRead": func() error { _, err := l.OpenRead(ctx, "a.txt"); return err },
		"WriteFile": func() error {
			return l.WriteFile(ctx, "b.txt", strings.NewReader("x"), 1, 1)
		},
		"Rename":  func() error { return l.Rename(ctx, "a.txt", "c.txt") },
		"Delete":  func() error { return l.Delete(ctx, "a.txt") },
		"MakeDir": func() error { return l.MakeDir(ctx, "d") },
	}
	for op, fn := range ops {
		if err := fn(); err == nil {
			t.Fatalf("操作 %s 在 ctx 取消时应快速失败", op)
		}
	}
}

// TestLocalFS_SymlinkEscape 验证 Root 内符号链接指向外部时文件操作被拒绝
// （审查 MEDIUM：symlink 逃逸 / root confinement 闭环）。
func TestLocalFS_SymlinkEscape(t *testing.T) {
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(outDir, filepath.Join(root, "escape")); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}
	l := NewLocalFS(root, nil)
	ctx := context.Background()

	// 读逃逸
	if _, err := l.Stat(ctx, "escape/secret.txt"); err == nil {
		t.Fatalf("Stat 逃逸应被拒绝")
	}
	if _, err := l.OpenRead(ctx, "escape/secret.txt"); err == nil {
		t.Fatalf("OpenRead 逃逸应被拒绝")
	}
	if _, err := l.ListDir(ctx, "escape"); err == nil {
		t.Fatalf("ListDir 逃逸应被拒绝")
	}
	// 写逃逸
	if err := l.WriteFile(ctx, "escape/evil.txt", strings.NewReader("x"), 1, 1); err == nil {
		t.Fatalf("WriteFile 逃逸应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outDir, "evil.txt")); err == nil {
		t.Fatalf("逃逸写入了外部文件")
	}
	// rename / delete 逃逸
	if err := l.Rename(ctx, "escape/secret.txt", "inside.txt"); err == nil {
		t.Fatalf("Rename 逃逸应被拒绝")
	}
	if err := l.Delete(ctx, "escape/secret.txt"); err == nil {
		t.Fatalf("Delete 逃逸应被拒绝")
	}
	// 外部文件不应被删除
	if _, err := os.Stat(filepath.Join(outDir, "secret.txt")); err != nil {
		t.Fatalf("外部 secret.txt 不应被删除: %v", err)
	}
}

// TestLocalFS_SymlinkInsideRootOK 验证 Root 内符号链接指向 Root 内部时允许（合法）。
func TestLocalFS_SymlinkInsideRootOK(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}
	l := NewLocalFS(root, nil)
	e, err := l.Stat(context.Background(), "link/a.txt")
	if err != nil || e == nil {
		t.Fatalf("Root 内符号链接应允许，err=%v", err)
	}
	if e.Size != 5 {
		t.Fatalf("大小不符: %d", e.Size)
	}
	// 写入 Root 内符号链接目标也允许
	if err := l.WriteFile(context.Background(), "link/b.txt", strings.NewReader("x"), 1, 1); err != nil {
		t.Fatalf("Root 内符号链接写入应允许: %v", err)
	}
}
