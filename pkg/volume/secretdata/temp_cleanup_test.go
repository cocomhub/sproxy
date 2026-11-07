// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupStaleTempDirs 孤儿临时目录（超龄）必须被清理，新鲜目录必须保留。
// 变异：去掉 ModTime 判断 ⇒ 新目录被删 ⇒ 红。
func TestCleanupStaleTempDirs(t *testing.T) {
	// 隔离 os.TempDir：t.Setenv 影响 os.MkdirTemp/os.TempDir
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("TMP", base)
	t.Setenv("TEMP", base)

	parent := filepath.Join(base, tempRootName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(parent, "111-old")
	freshDir := filepath.Join(parent, "222-fresh")
	for _, d := range []string{oldDir, freshDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// 旧散落格式（旧实现直接在 Temp 下建）
	legacy := filepath.Join(base, "sproxy-secretdata-123456")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-staleTempMaxAge - time.Hour)
	for _, d := range []string{oldDir, legacy} {
		if err := os.Chtimes(d, stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	cleanupStaleTempDirs(parent)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("超龄目录应被清理: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("旧散落残留应被清理: %v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Fatalf("新鲜目录不应被清理: %v", err)
	}
}

// TestNewDefaultTempDir 默认临时目录必须落在固定父目录下（不再散落在 Temp 根）。
func TestNewDefaultTempDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("TMP", base)
	t.Setenv("TEMP", base)
	dir, err := newDefaultTempDir()
	if err != nil {
		t.Fatalf("newDefaultTempDir: %v", err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != filepath.Join(base, tempRootName) {
		t.Fatalf("应落在固定父目录下，实际 %s", dir)
	}
}
