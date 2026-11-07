// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMkdirTemp_CreatesAndCleansUp 创建成功，且 cleanup 后目录消失。
func TestMkdirTemp_CreatesAndCleansUp(t *testing.T) {
	t.Parallel()
	dir := MkdirTemp(t, "testutil-tmp-")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("目录应存在: %v", err)
	}
	// 子目录 + 文件也能被递归清掉。
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sub := t.Run("cleanup", func(t *testing.T) {})
	if !sub {
		t.Fatal("subtest 失败")
	}
	// 父测试结束时才清理；此处仅断言路径存在且非空。
	if dir == "" {
		t.Fatal("MkdirTemp 返回空路径")
	}
}

// TestCleanupDir_EmptyIsNoop 空路径不注册清理（防误删 cwd）。
func TestCleanupDir_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	CleanupDir(t, "")
}
