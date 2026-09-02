// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRoot_RejectsTraversal os.Root 防穿越：.. / 绝对路径 / Windows 反斜杠穿越。
func TestRoot_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	r, openErr := OpenRoot(dir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer r.Close()
	for _, rel := range []string{"../etc/passwd", "a/../../etc", "/abs", `..\escape`} {
		if _, err := r.Open(rel); err == nil {
			t.Fatalf("穿越路径 %q 应被拒绝", rel)
		}
	}
	if err := r.MkdirAll("user/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := r.OpenFile("user/sub/f.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := r.Open("user/sub/f.txt"); err != nil {
		t.Fatalf("合法路径应可读: %v", err)
	}
}

// TestRoot_SymlinkEscape 验证指向 root 外的符号链接被 os.Root 拒绝。
// 平台不支持创建符号链接（如 Windows 无管理员/开发者模式）时跳过。
func TestRoot_SymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "escape-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("平台不支持创建符号链接（需管理员/开发者模式）: %v", err)
	}
	if _, err := r.Open("escape-link"); err == nil {
		t.Fatalf("符号链接指向 root 外应被拒绝")
	}
}

// TestOpenRoot_LayoutVersion 验证 LAYOUT_VERSION 自动写入与不匹配时迁移钩子报错。
func TestOpenRoot_LayoutVersion(t *testing.T) {
	dir := t.TempDir()
	r, openErr := OpenRoot(dir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "LAYOUT_VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != LayoutVersion {
		t.Fatalf("LAYOUT_VERSION=%q want %q", got, LayoutVersion)
	}

	// 内容不匹配 → 迁移钩子报错
	if err := os.WriteFile(filepath.Join(dir, "LAYOUT_VERSION"), []byte("999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRoot(dir); err == nil {
		t.Fatalf("LAYOUT_VERSION 不匹配应报错")
	}
}

// TestRoot_MkdirAllAndChtimes 验证 MkdirAll 幂等与 Chtimes 生效。
func TestRoot_MkdirAllAndChtimes(t *testing.T) {
	dir := t.TempDir()
	r, openErr := OpenRoot(dir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer r.Close()

	if err := r.MkdirAll("a/b/c", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAll("a/b/c", 0o755); err != nil {
		t.Fatalf("已存在目录应幂等: %v", err)
	}
	if fi, err := r.Stat("a/b/c"); err != nil || !fi.IsDir() {
		t.Fatalf("a/b/c 应为目录: %v", err)
	}

	f, err := r.OpenFile("a/b/f.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	mtime := time.Now().Add(-time.Hour)
	if ctErr := r.Chtimes("a/b/f.txt", mtime, mtime); ctErr != nil {
		t.Fatal(ctErr)
	}
	fi, err := r.Stat("a/b/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d := fi.ModTime().Sub(mtime); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("ModTime delta=%v 超出容差（文件系统时间粒度）", d)
	}
}

// TestRoot_MkdirAll_NonDir 验证中间层是文件时 MkdirAll 报错。
func TestRoot_MkdirAll_NonDir(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	f, err := r.OpenFile("block", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := r.MkdirAll("block/sub", 0o755); err == nil {
		t.Fatalf("在文件下创建子目录应报错")
	}
}

// TestRoot_Abs 验证 Abs 派生绝对路径与拒绝穿越。
func TestRoot_Abs(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	abs, ok := r.Abs("user/f.txt")
	if !ok {
		t.Fatalf("Abs(user/f.txt) 应 ok")
	}
	if abs != filepath.Join(dir, "user", "f.txt") {
		t.Fatalf("Abs=%q want %q", abs, filepath.Join(dir, "user", "f.txt"))
	}

	if got, ok := r.Abs(""); !ok || got != dir {
		t.Fatalf("Abs(空)=%q,%v want base %q", got, ok, dir)
	}
	for _, rel := range []string{"../x", "a/../../etc", "/abs", `..\x`, `..\escape`} {
		if _, ok := r.Abs(rel); ok {
			t.Fatalf("Abs(%q) 应拒绝", rel)
		}
	}
}
