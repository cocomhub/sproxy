// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tempclean

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupStale_FixedParent 固定父目录下超龄子目录被清理并统计字节数；新鲜项保留。
// 变异：去掉 ModTime 判定 ⇒ 新鲜项被删 ⇒ 红。
func TestCleanupStale_FixedParent(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("TMP", base)
	t.Setenv("TEMP", base)

	parent := filepath.Join(base, RootSecretdata)
	oldDir := filepath.Join(parent, "111-old")
	newDir := filepath.Join(parent, "222-new")
	for _, d := range []string{oldDir, newDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// 旧目录里放 4KB 内容（验证 FreedBytes）
	payload := make([]byte, 4096)
	if err := os.WriteFile(filepath.Join(oldDir, "chunk"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-DefaultMaxAge - time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}

	rep := CleanupStale(DefaultMaxAge)
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("超龄目录应被清理: %v", err)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatalf("新鲜目录不应被清理: %v", err)
	}
	if rep.Removed != 1 || rep.FreedBytes < 4096 {
		t.Fatalf("统计不正确: %+v", rep)
	}
}

// TestCleanupStale_LegacyAndMultipart 历史散落前缀（sproxy-secretdata-* / sproxy-sig-body-* /
// multipart-*）超龄项必须被清理（这些不在固定父目录下）。
func TestCleanupStale_LegacyAndMultipart(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("TMP", base)
	t.Setenv("TEMP", base)

	targets := []string{
		filepath.Join(base, "sproxy-secretdata-123"),
		filepath.Join(base, "sproxy-sig-body-456"),
		filepath.Join(base, "multipart-789"),
	}
	stale := time.Now().Add(-DefaultMaxAge - time.Hour)
	for _, p := range targets {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	// 无关目录必须保留
	other := filepath.Join(base, "unrelated-dir")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, stale, stale); err != nil {
		t.Fatal(err)
	}

	rep := CleanupStale(DefaultMaxAge)
	for _, p := range targets {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("历史残留应被清理: %s", p)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("非 sproxy 目录不应被清理: %v", err)
	}
	if rep.Removed != len(targets) {
		t.Fatalf("removed=%d want=%d", rep.Removed, len(targets))
	}
}

// TestResidualBytes 只读统计：不清删，返回残留字节数与项数（供配额对账维度）。
func TestResidualBytes(t *testing.T) {
	base := t.TempDir()
	t.Setenv("TMPDIR", base)
	t.Setenv("TMP", base)
	t.Setenv("TEMP", base)

	parent := filepath.Join(base, RootSecretdata)
	dir := filepath.Join(parent, "p1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 2048)
	if err := os.WriteFile(filepath.Join(dir, "chunk"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(base, "multipart-1")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "f"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	bytes, items := ResidualBytes()
	if items != 2 || bytes < 4096 {
		t.Fatalf("统计不正确: bytes=%d items=%d", bytes, items)
	}
	// 只读：不得删除
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("ResidualBytes 不应删除任何内容: %v", err)
	}
}
