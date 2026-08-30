// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"reflect"
	"testing"
)

// TestParseFilters 验证 include/exclude 解析为 Filter 列表。
func TestParseFilters(t *testing.T) {
	filters := ParseFilters([]string{"*.go", "*.md"}, []string{"vendor", "*.tmp"})
	if len(filters) != 4 {
		t.Fatalf("期望 4 个 filter，got %d", len(filters))
	}
	if filters[0] != (Filter{Pattern: "*.go"}) {
		t.Fatalf("第一个 include 应为 *.go，got %+v", filters[0])
	}
	if filters[1] != (Filter{Pattern: "*.md"}) {
		t.Fatalf("第二个 include 应为 *.md，got %+v", filters[1])
	}
	if filters[2] != (Filter{Pattern: "vendor", Exclude: true}) {
		t.Fatalf("第一个 exclude 应为 vendor/Exclude=true，got %+v", filters[2])
	}
	if filters[3] != (Filter{Pattern: "*.tmp", Exclude: true}) {
		t.Fatalf("第二个 exclude 应为 *.tmp/Exclude=true，got %+v", filters[3])
	}
}

// TestParseFilters_EmptySkipped 验证空 pattern 被跳过。
func TestParseFilters_EmptySkipped(t *testing.T) {
	filters := ParseFilters([]string{"", "*.go", "  "}, []string{""})
	if len(filters) != 1 {
		t.Fatalf("空 pattern 应被跳过，got %d", len(filters))
	}
	if filters[0] != (Filter{Pattern: "*.go"}) {
		t.Fatalf("got %+v", filters[0])
	}
}

// TestParseFilters_Nil 验证 nil/空输入返回空切片。
func TestParseFilters_Nil(t *testing.T) {
	if f := ParseFilters(nil, nil); len(f) != 0 {
		t.Fatalf("nil 输入应返回空切片，got %d", len(f))
	}
}

// TestMatchFilters 验证 include/exclude 优先级与空过滤器语义。
func TestMatchFilters(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		filters []Filter
		want    bool
	}{
		{"无过滤器→放行", "a.txt", nil, true},
		{"空过滤器→放行", "a.txt", []Filter{}, true},
		{"exclude 命中→拒绝", "a.tmp", []Filter{{Pattern: "*.tmp", Exclude: true}}, false},
		{"exclude 未命中→放行", "a.go", []Filter{{Pattern: "*.tmp", Exclude: true}}, true},
		{"include 命中→放行", "a.go", []Filter{{Pattern: "*.go"}}, true},
		{"include 未命中→拒绝", "a.txt", []Filter{{Pattern: "*.go"}}, false},
		{"无 include 时 exclude 未命中→放行", "a.txt", []Filter{{Pattern: "vendor", Exclude: true}}, true},
		{"exclude 优先于 include（同时命中）→拒绝", "a.tmp", []Filter{{Pattern: "*.tmp"}, {Pattern: "*.tmp", Exclude: true}}, false},
		{"include 命中且 exclude 未命中→放行", "a.go", []Filter{{Pattern: "*.tmp", Exclude: true}, {Pattern: "*.go"}}, true},
		{"多 include 任一命中→放行", "b.md", []Filter{{Pattern: "*.go"}, {Pattern: "*.md"}}, true},
		{"空 pattern 被忽略", "a.txt", []Filter{{Pattern: ""}}, true},
		{"非法 pattern 被忽略（不产生 include）", "a.txt", []Filter{{Pattern: "["}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchFilters(tc.path, tc.filters)
			if got != tc.want {
				t.Fatalf("MatchFilters(%q) 应为 %v，got %v", tc.path, tc.want, got)
			}
		})
	}
}

// TestMatchFilters_SubdirPath 验证对子目录相对路径的匹配（递归枚举的语义）。
func TestMatchFilters_SubdirPath(t *testing.T) {
	// 不含分隔符的 pattern 同时匹配 basename（rsync 风格，审查 I-1）：
	// `*.go` 应匹配 sub/file.go 的 basename。
	if !MatchFilters("sub/file.go", []Filter{{Pattern: "*.go"}}) {
		t.Fatalf("*.go 应匹配含分隔符路径的 basename sub/file.go")
	}
	// 含分隔符的 pattern 保持完整路径匹配。
	if !MatchFilters("sub/file.go", []Filter{{Pattern: "sub/*.go"}}) {
		t.Fatalf("sub/*.go 应匹配 sub/file.go")
	}
	// exclude 同理按 basename 排除任意层级。
	if MatchFilters("sub/a.tmp", []Filter{{Pattern: "*.tmp", Exclude: true}}) {
		t.Fatalf("*.tmp exclude 应排除 sub/a.tmp")
	}
}

// TestMatchFiltersDir 验证目录条目只受 exclude 约束（include 不阻断递归，审查 I-1）。
func TestMatchFiltersDir(t *testing.T) {
	// exclude 命中目录 → 剪枝
	if MatchFiltersDir("sub", ParseFilters(nil, []string{"sub"})) {
		t.Fatalf("exclude 命中目录 sub 应剪枝")
	}
	// *.tmp 匹配文件 basename，不匹配目录名 sub → 不剪枝
	if !MatchFiltersDir("sub", ParseFilters(nil, []string{"*.tmp"})) {
		t.Fatalf("*.tmp 不匹配目录名 sub，不应剪枝")
	}
	// include 不阻断目录递归
	if !MatchFiltersDir("sub", ParseFilters([]string{"*.go"}, nil)) {
		t.Fatalf("include 不应剪枝目录 sub（深层文件仍需枚举）")
	}
	if !MatchFiltersDir("sub", ParseFilters([]string{"sub/*.go"}, nil)) {
		t.Fatalf("include 不应剪枝目录 sub")
	}
	// 无过滤器全部放行
	if !MatchFiltersDir("sub", nil) {
		t.Fatalf("无过滤器目录应放行")
	}
}

// TestWalkEntries_IncludeDoesNotPruneSubtree 验证 include 过滤器不把整棵子树剪掉
// （审查 I-1 回归：--include "*.go" 应递归包含 sub/x.go）。
func TestWalkEntries_IncludeDoesNotPruneSubtree(t *testing.T) {
	m := newMockFS()
	m.setFile("a.go", []byte("a"), 1)
	m.setFile("sub/x.go", []byte("x"), 2)
	m.setFile("sub/b.txt", []byte("b"), 3)
	m.setFile("sub/deep/y.go", []byte("y"), 4)

	entries, err := WalkEntries(context.Background(), m, "", true, false, ParseFilters([]string{"*.go"}, nil))
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.go", "sub/deep/y.go", "sub/x.go"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("include *.go 应递归包含深层 .go 文件\n got: %v\nwant: %v", got, want)
	}
}

// TestWalkEntries_ExcludePrunesSubtree 验证 exclude 命中目录时整棵子树被剪枝。
func TestWalkEntries_ExcludePrunesSubtree(t *testing.T) {
	m := newMockFS()
	m.setFile("a.txt", []byte("a"), 1)
	m.setFile("skip/b.txt", []byte("b"), 2)
	m.setFile("skip/deep/c.txt", []byte("c"), 3)

	entries, err := WalkEntries(context.Background(), m, "", true, false, ParseFilters(nil, []string{"skip"}))
	if err != nil {
		t.Fatalf("WalkEntries error: %v", err)
	}
	want := []string{"a.txt"}
	if got := walkPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("exclude 目录应整树剪枝\n got: %v\nwant: %v", got, want)
	}
}
