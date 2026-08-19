// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultFromURL_FromFixture 以共享语料 testdata/cases.json 为基准验证 DefaultFromURL。
// 该语料同时被 Web UI 的 JS 单测 (web/static/cloudfilename.test.js) 复用，
// 保证 Go 服务端与浏览器端对同一 URL 推导出完全一致的默认文件名。
func TestDefaultFromURL_FromFixture(t *testing.T) {
	cases := loadFixture(t)
	for url, want := range cases {
		if got := DefaultFromURL(url); got != want {
			t.Errorf("DefaultFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestDefaultFromURL_KeyRules 对关键 wget 规则做显式断言，便于读测试即理解行为。
func TestDefaultFromURL_KeyRules(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "普通文件", url: "https://example.com/file.txt", want: "file.txt"},
		{name: "多级目录取最后一段", url: "https://example.com/a/b/c.jpg", want: "c.jpg"},
		{name: "根路径", url: "https://example.com/", want: "index.html"},
		{name: "根路径带查询", url: "https://example.com/?a=v", want: "index.html?a=v"},
		{name: "目录结尾使用index.html", url: "https://example.com/foo/", want: "index.html"},
		{name: "目录结尾带查询", url: "https://example.com/xx/?a=v", want: "index.html?a=v"},
		{name: "查询参数直接附加", url: "https://example.com/file.txt?token=abc&x=1", want: "file.txt?token=abc&x=1"},
		{name: "百分号解码", url: "https://example.com/my%20file.txt", want: "my file.txt"},
		{name: "百分号解码不转加号", url: "https://example.com/a+b.txt", want: "a+b.txt"},
		{name: "双重编码", url: "https://example.com/a%2520b.txt", want: "a b.txt"},
		{name: "无路径", url: "https://example.com", want: "index.html"},
		{name: "无路径带查询", url: "https://example.com?a=v", want: "index.html?a=v"},
		{name: "非法百分号编码回退download", url: "https://example.com/100%.txt", want: "download"},
		{name: "中文路径解码", url: "https://example.com/%E4%B8%AD%E6%96%87.txt", want: "中文.txt"},
		{name: "无效URL", url: "not a url", want: "download"},
		{name: "相对URL无host", url: "example.com/file.txt", want: "download"},
		{name: "空串", url: "", want: "download"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultFromURL(tt.url); got != tt.want {
				t.Errorf("DefaultFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestSafe(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "普通文件名不变", in: "file.txt", want: "file.txt"},
		{name: "反斜杠替换", in: `a\b.txt`, want: "a_b.txt"},
		{name: "斜杠替换", in: "a/b.txt", want: "a_b.txt"},
		{name: "问号替换", in: "a?b.txt", want: "a_b.txt"},
		{name: "冒号替换", in: "a:b.txt", want: "a_b.txt"},
		{name: "尖括号替换", in: "a<b>c", want: "a_b_c"},
		{name: "竖线替换", in: "a|b", want: "a_b"},
		{name: "双引号替换", in: `a"b`, want: "a_b"},
		{name: "星号替换", in: "a*b", want: "a_b"},
		{name: "NUL移除", in: "a\x00b", want: "ab"},
		{name: "组合替换", in: `a/b?c:d"e*f|g<h>i`, want: "a_b_c_d_e_f_g_h_i"},
		{name: "等号与与号保留", in: "file.txt?a=b&c=d", want: "file.txt_a=b&c=d"},
		{name: "首尾空白去除", in: "  file.txt  ", want: "file.txt"},
		{name: "首尾点去除", in: "..file.txt..", want: "file.txt"},
		{name: "仅点与空白", in: "... ...", want: "download"},
		{name: "空串", in: "", want: "download"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Safe(tt.in); got != tt.want {
				t.Errorf("Safe(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestDefaultFromURLThenSafe 验证"生成 + 清理"的完整链路（与 server 端一致）。
func TestDefaultFromURLThenSafe(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{url: "https://example.com/xx/?a=v", want: "index.html_a=v"},
		{url: "https://example.com/a%2Fb.txt", want: "b.txt"},
		{url: "https://example.com/my%20file.txt", want: "my file.txt"},
		{url: "https://example.com/path/file.txt?x=1", want: "file.txt_x=1"},
	}
	for _, tt := range tests {
		if got := Safe(DefaultFromURL(tt.url)); got != tt.want {
			t.Errorf("Safe(DefaultFromURL(%q)) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// loadFixture 读取共享语料 testdata/cases.json。
func loadFixture(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var cases map[string]string
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	return cases
}
