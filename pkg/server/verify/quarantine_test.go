// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestQuarantineBadFile 坏文件隔离：移到租户 meta/quarantine/ 保留现场（不删除）。
func TestQuarantineBadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	const rel = "user/docs/a.txt"
	if err := os.MkdirAll(filepath.Join(dir, "user", "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("bad"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := quarantineBadFile(root, rel); err != nil {
		t.Fatalf("quarantineBadFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("原文件应已移走: %v", err)
	}
	matches, gerr := filepath.Glob(filepath.Join(dir, "meta", "quarantine", "**", "a.txt*"))
	if gerr != nil || len(matches) == 0 {
		// 隔离路径可能是扁平化命名；退化为扫描 quarantine 目录非空。
		entries, derr := os.ReadDir(filepath.Join(dir, "meta", "quarantine"))
		if derr != nil || len(entries) == 0 {
			t.Fatalf("隔离目录应有文件（glob=%v err=%v, readdir=%v err=%v）", matches, gerr, entries, derr)
		}
	}
}
