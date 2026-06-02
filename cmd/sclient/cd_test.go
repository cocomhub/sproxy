// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestResolveRemotePath(t *testing.T) {
	// 不并行：测试操作包级变量 currentDir，子测试串行。
	cases := []struct {
		name       string
		currentDir string
		input      string
		want       string
		wantErr    bool
	}{
		{"empty_input_root", "", "", "", false},
		{"empty_input_with_currentdir", "sub", "", "sub", false},
		{"relative_root", "", "file.txt", "file.txt", false},
		{"relative_with_currentdir", "sub", "file.txt", "sub/file.txt", false},
		{"absolute_skips_currentdir", "sub", "/abs/file.txt", "abs/file.txt", false},
		{"trailing_slash_cleaned", "sub", "file.txt/", "sub/file.txt", false},
		{"dot_no_op", "sub", ".", "sub", false},
		{"reject_parent_top", "", "..", "", true},
		{"reject_parent_relative", "", "../etc/passwd", "", true},
		{"reject_parent_after_currentdir_resolve", "sub", "../../etc", "", true},
		// currentDir=sub + ../foo → 经 Clean 后变成 foo，没有 `..` 残留，是合法路径。
		{"parent_cancels_currentdir_one_level", "sub", "../foo", "foo", false},
		// Clean 后内部 `..` 已被消除：a/../b → b，是合法的
		{"internal_parent_cleaned_to_safe", "", "a/../b", "b", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := currentDir
			currentDir = c.currentDir
			defer func() { currentDir = old }()

			got, err := resolveRemotePath(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q (currentDir=%q), got nil (returned %q)", c.input, c.currentDir, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("want %q, got %q", c.want, got)
			}
		})
	}
}

func TestResolveRemotePath_ParentRefMessage(t *testing.T) {
	old := currentDir
	currentDir = ""
	defer func() { currentDir = old }()

	_, err := resolveRemotePath("../escape.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "父级引用") {
		t.Fatalf("error message should mention parent ref: %v", err)
	}
}
