// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// tree_test.go 钉住 tree 的**递归渲染语义**（Linux 习惯输出）：目录在前并带 `/`、分支符号、
// 深度限制、dirs-only、结尾统计行。用内存树注入（treeLister 接缝）⇒ 与网络/卷无关。
func TestWalkTree_RendersLinuxStyleTree(t *testing.T) {
	t.Parallel()
	tree := map[string][]client.FileInfo{
		"/a": {
			{Name: "x.bin", Size: 10},
			{Name: "b", IsDir: true},
			{Name: "c.txt", Size: 20},
		},
		"/a/b": {{Name: "deep.txt", Size: 5}},
	}
	list := func(dir string) ([]client.FileInfo, error) { return tree[dir], nil }

	var out bytes.Buffer
	var st treeStats
	if err := walkTree(list, "/a", "", 0, false, &out, &st); err != nil {
		t.Fatalf("walkTree: %v", err)
	}
	got := out.String()
	// 目录优先（b/ 在 c.txt 前），缩进与分支符号正确。
	want := "├── b/\n" +
		"│   └── deep.txt\n" +
		"├── c.txt\n" +
		"└── x.bin\n"
	if got != want {
		t.Fatalf("tree 渲染不符:\n got=%q\nwant=%q", got, want)
	}
	if st.dirs != 1 || st.files != 3 {
		t.Fatalf("统计 dirs=%d files=%d，want 1/3", st.dirs, st.files)
	}

	// --depth 1：只列当前层（不递归）。
	out.Reset()
	st = treeStats{}
	if err := walkTree(list, "/a", "", 1, false, &out, &st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "deep.txt") {
		t.Fatalf("depth=1 不应递归: %q", out.String())
	}
	// --dirs-only：只显示目录。
	out.Reset()
	st = treeStats{}
	if err := walkTree(list, "/a", "", 0, true, &out, &st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "c.txt") || strings.Contains(out.String(), "x.bin") {
		t.Fatalf("dirs-only 不应显示文件: %q", out.String())
	}
	if !strings.Contains(out.String(), "b/") {
		t.Fatalf("dirs-only 应显示目录: %q", out.String())
	}
}

// TestTreeCmd_RegisteredWithURIHelp：tree 命令已注册、支持卷 URI 形态的用法说明。
func TestTreeCmd_RegisteredWithURIHelp(t *testing.T) {
	t.Parallel()
	cmd := NewCmdTree(nil, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	if cmd.Use != "tree [path]" {
		t.Fatalf("Use=%q", cmd.Use)
	}
	if !strings.Contains(cmd.Long, "卷 URI") {
		t.Fatalf("帮助应说明卷 URI 寻址:\n%s", cmd.Long)
	}
}
