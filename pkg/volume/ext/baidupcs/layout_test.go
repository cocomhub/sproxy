// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLayout_Structure 验证 NewLayout 创建全部子目录。
func TestLayout_Structure(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	l, err := NewLayout(base)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	for name, dir := range map[string]string{
		"BaseDir": l.BaseDir, "Staging": l.Staging, "Resume": l.Resume,
		"Cache": l.Cache, "Tmp": l.Tmp,
	} {
		if dir == "" {
			t.Fatalf("%s 不应为空", name)
		}
		fi, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("%s 目录不存在: %v", name, statErr)
		}
		if !fi.IsDir() {
			t.Fatalf("%s 不是目录", name)
		}
	}
	if l.BaseDir != filepath.Clean(base) {
		t.Fatalf("BaseDir = %q, want %q", l.BaseDir, filepath.Clean(base))
	}
}

// TestLayout_SanitizeKey 验证路径穿越形状被归一为安全文件名。
func TestLayout_SanitizeKey(t *testing.T) {
	t.Parallel()
	l := &Layout{BaseDir: t.TempDir()}
	cases := []struct {
		in   string
		want string
	}{
		{"f.txt", "f.txt"},
		{"a/b/c.txt", "a_b_c.txt"},
		{"../etc/passwd", "_._etc_passwd"},
		{"..", "_."},
		{"", "_"},
		{"a\\b", "a_b"},
	}
	for _, tc := range cases {
		if got := l.SanitizeKey(tc.in); got != tc.want {
			t.Fatalf("SanitizeKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLayout_SanitizeKey_NoEscape 验证归一后的 key 不会越出 base。
func TestLayout_SanitizeKey_NoEscape(t *testing.T) {
	t.Parallel()
	l := &Layout{BaseDir: t.TempDir()}
	key := l.SanitizeKey("../../../../etc/passwd")
	if filepath.IsAbs(key) {
		t.Fatalf("归一 key 不应为绝对路径: %q", key)
	}
	joined := filepath.Join(l.BaseDir, key)
	rel, relErr := filepath.Rel(l.BaseDir, joined)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("归一 key 越出 base: %q (rel=%q)", joined, rel)
	}
}
