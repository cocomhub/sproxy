// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"runtime"
	"strings"
	"testing"
)

func TestValidateFilePath_HappyCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "file.txt", "file.txt"},
		{"subdir_unix", "dir/sub/file.txt", "dir/sub/file.txt"},
		{"clean_dot_slash", "./file.txt", "file.txt"},
		{"clean_redundant_slash", "a//b//c.txt", "a/b/c.txt"},
		{"chinese", "中文目录/中文文件.txt", "中文目录/中文文件.txt"},
		{"with_spaces", "a b/c d.txt", "a b/c d.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ValidateFilePath(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestValidateFilePath_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"null_byte", "abc\x00.txt"},
		{"absolute_unix", "/etc/passwd"},
		{"absolute_backslash", "\\foo\\bar"},
		{"parent_ref_top", ".."},
		{"parent_ref_relative", "../etc/passwd"},
		{"only_dot", "."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateFilePath(c.in); err == nil {
				t.Fatalf("expected error for %q, got nil", c.in)
			}
		})
	}
}

// TestValidateFilePath_CleansInternalParentRef 验证 ValidateFilePath 不强制拦截
// 经 filepath.Clean 能消除的内部 `..`（如 a/../b 等价于 b）：
// 这是设计行为——只要清洗后没有穿越实际就是安全的。
func TestValidateFilePath_CleansInternalParentRef(t *testing.T) {
	t.Parallel()
	got, err := ValidateFilePath("a/../b/c.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b/c.txt" {
		t.Fatalf("want b/c.txt, got %q", got)
	}
}

func TestValidateFilePath_WindowsInvalidChars(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows enforces these illegal chars")
	}
	t.Parallel()
	const invalid = `<>:"|?*`
	for _, c := range invalid {
		ch := c // capture
		t.Run(string(ch), func(t *testing.T) {
			t.Parallel()
			if _, err := ValidateFilePath("a" + string(ch) + "b.txt"); err == nil {
				t.Fatalf("expected error for char %q on Windows", ch)
			}
		})
	}
}

func TestValidateFilePath_LongName(t *testing.T) {
	t.Parallel()
	// 不强制总长上限（取决于 OS），这里仅确认超长名不会 panic 且返回结果一致。
	long := strings.Repeat("a", 4096) + ".txt"
	got, err := ValidateFilePath(long)
	if err != nil {
		// 某些 OS 可能因 filename 过长而由 Clean 拒绝；只要不 panic 就行
		return
	}
	if got != long {
		t.Fatalf("long name should be preserved verbatim, got %q", got)
	}
}

// TestValidateFilePath_RejectsInternalDirPrefix 验证（审查 I-1 多租户 owner）：
// 首段以 .__ 开头的路径被拒绝——服务端内部目录约定（.__cloud__/.__versions__ 等），
// 云任务文件落在 .__cloud__/<taskID>/<file>，拦截防止跨租户经 /download 读取。
func TestValidateFilePath_RejectsInternalDirPrefix(t *testing.T) {
	for _, f := range []string{
		".__cloud__/task123/file.zip",
		".__versions__/x.txt",
		".__sync__/y.txt",
		".__downloads__/z.txt",
		".__cloud__",
	} {
		if _, err := ValidateFilePath(f); err == nil {
			t.Errorf("首段 .__ 路径 %q 应被拒绝", f)
		}
	}
	// 深层含 .__ 的普通用户文件仍允许（只拦首段）。
	for _, f := range []string{
		"dir/foo.__bar.txt",
		"normal.txt",
		"dir/sub/file.txt",
	} {
		if _, err := ValidateFilePath(f); err != nil {
			t.Errorf("普通路径 %q 应允许，got %v", f, err)
		}
	}
}
