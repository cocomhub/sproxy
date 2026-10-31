// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package archive

// archive_permission_unix_test.go 覆盖「Lstat 成功但 open / ReadDir 失败」两条分支。
//
// 为什么只在 Unix：Windows 的目录/文件权限位（只读位）不足以让 Lstat 之后的操作失败，
// 且 Go 的 os.Root 打开句柄带共享模式 ⇒ 无法用共享锁只挡住 open 而放过 Lstat。
// Unix 上用 0o000 权限位可稳定构造：Lstat（只读父目录 inode）成功，open/ReadDir 返回 EACCES。

import (
	"archive/tar"
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAddFileToTar_OpenFailure Lstat 成功但文件不可读 → 打开文件失败。
func TestAddFileToTar_OpenFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 无视文件权限位，无法构造 open 失败")
	}
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/locked.txt", "abc")
	locked := filepath.Join(dir, testUserDir, "locked.txt")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod 不可用: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	var buf bytes.Buffer
	err := AddFileToTar(tar.NewWriter(&buf), root, testUserDir+"/locked.txt", testTarRoot+"/locked.txt", slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "打开文件失败") {
		t.Fatalf("err = %v，want 打开文件失败", err)
	}
}

// TestAddFileToTar_ReadDirFailure Lstat 成功但目录不可读 → 读取目录失败。
func TestAddFileToTar_ReadDirFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root 无视文件权限位，无法构造 ReadDir 失败")
	}
	root, dir := newArchiveTestRoot(t)
	writeCoverageFile(t, dir, testUserDir+"/inner/"+testNoteFile, "abc")
	lockedDir := filepath.Join(dir, testUserDir)
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Skipf("chmod 不可用: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })

	var buf bytes.Buffer
	err := AddFileToTar(tar.NewWriter(&buf), root, testUserDir, testTarRoot, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "读取目录失败") {
		t.Fatalf("err = %v，want 读取目录失败", err)
	}
}
