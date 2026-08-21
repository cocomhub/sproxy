// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudfilename

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		{name: "根路径带查询", url: "https://example.com/?a=v", want: "index.html_a=v"},
		{name: "目录结尾使用index.html", url: "https://example.com/foo/", want: "index.html"},
		{name: "目录结尾带查询", url: "https://example.com/xx/?a=v", want: "index.html_a=v"},
		{name: "查询参数直接附加", url: "https://example.com/file.txt?token=abc&x=1", want: "file.txt_token=abc&x=1"},
		{name: "百分号解码", url: "https://example.com/my%20file.txt", want: "my file.txt"},
		{name: "百分号解码不转加号", url: "https://example.com/a+b.txt", want: "a+b.txt"},
		{name: "双重编码", url: "https://example.com/a%2520b.txt", want: "a b.txt"},
		{name: "无路径", url: "https://example.com", want: "index.html"},
		{name: "无路径带查询", url: "https://example.com?a=v", want: "index.html_a=v"},
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

func TestResolveFilename_ExplicitValid(t *testing.T) {
	got, err := ResolveFilename(Entry{URL: "https://e.com/a.zip", Filename: "valid.zip"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "valid.zip" {
		t.Fatalf("want valid.zip, got %q", got)
	}
}

func TestResolveFilename_ExplicitUnsafe(t *testing.T) {
	_, err := ResolveFilename(Entry{URL: "https://e.com/a.zip", Filename: "a/b.zip"})
	if err == nil {
		t.Fatal("expected error for unsafe filename")
	}
	if !errors.Is(err, ErrEntryUnsafeFilename) {
		t.Fatalf("want ErrEntryUnsafeFilename, got %v", err)
	}
}

func TestResolveFilename_AutoFromURL(t *testing.T) {
	got, err := ResolveFilename(Entry{URL: "https://e.com/xx/?a=v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// DefaultFromURL 现在是安全版，? 被替换为 _
	if got != "index.html_a=v" {
		t.Fatalf("want index.html_a=v, got %q", got)
	}
}

func TestValidateEntry(t *testing.T) {
	tests := []struct {
		name string
		e    Entry
		err  error
	}{
		{name: "空 URL", e: Entry{URL: ""}, err: ErrEntryEmptyURL},
		{name: "非法 scheme", e: Entry{URL: "ftp://e.com/a.zip"}, err: ErrEntryBadScheme},
		{name: "缺 host", e: Entry{URL: "http:///path"}, err: ErrEntryMissingHost},
		{name: "合法 URL", e: Entry{URL: "https://e.com/a.zip"}, err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateEntry(tt.e)
			if tt.err == nil && got != nil {
				t.Fatalf("unexpected error: %v", got)
			}
			if tt.err != nil && !errors.Is(got, tt.err) {
				t.Fatalf("want %v, got %v", tt.err, got)
			}
		})
	}
}

func TestValidateEntries_DupURL_DiffFilename(t *testing.T) {
	entries := []Entry{
		{URL: "https://e.com/a.zip", Filename: "a.zip"},
		{URL: "https://e.com/a.zip", Filename: "b.zip"},
	}
	err := ValidateEntries(entries)
	if !errors.Is(err, ErrEntryDupURL) {
		t.Fatalf("want ErrEntryDupURL, got %v", err)
	}
}

func TestValidateEntries_DupURL_SameFilenameOK(t *testing.T) {
	entries := []Entry{
		{URL: "https://e.com/a.zip", Filename: "a.zip"},
		{URL: "https://e.com/a.zip", Filename: "a.zip"},
	}
	if err := ValidateEntries(entries); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEntries_Valid(t *testing.T) {
	entries := []Entry{
		{URL: "https://e.com/a.zip"},
		{URL: "https://e.com/b.zip", Filename: "b.zip"},
	}
	if err := ValidateEntries(entries); err != nil {
		t.Fatalf("unexpected error: %v", err)
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
		{name: "Tab替换", in: "a	b", want: "a_b"},
		{name: "NUL移除", in: "a\x00b", want: "ab"},
		{name: "组合替换", in: `a/b?c:d"e*f|g<h>i`, want: "a_b_c_d_e_f_g_h_i"},
		{name: "等号与与号保留", in: "file.txt?a=b&c=d", want: "file.txt_a=b&c=d"},
		{name: "首尾空白去除", in: "  file.txt  ", want: "file.txt"},
		{name: "首尾点去除", in: "..file.txt..", want: "file.txt"},
		{name: "仅点与空白", in: "... ...", want: "download"},
		{name: "空串", in: "", want: "download"},
		{name: "Windows保留名CON", in: "CON", want: "_CON"},
		{name: "Windows保留名小写带扩展", in: "con.txt", want: "_con.txt"},
		{name: "Windows保留名COM1", in: "COM1", want: "_COM1"},
		{name: "Windows保留名LPT9", in: "lpt9", want: "_lpt9"},
		{name: "CON后接字符不命中", in: "CONtext.txt", want: "CONtext.txt"},
		{name: "超长文件名截断保留扩展名", in: strings.Repeat("a", 300) + ".txt", want: strings.Repeat("a", 250) + ".txt"},
		{name: "超长多字节不劈开UTF-8", in: strings.Repeat("好", 100) + ".zip", want: strings.Repeat("好", 83) + ".zip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Safe(tt.in); got != tt.want {
				t.Errorf("Safe(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultFromURL_SafeOutput(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://e.com/path/file.txt?x=1&y=2", "file.txt_x=1&y=2"},
		{"https://e.com/a?b/c", "a_b_c"},
	}
	for _, tt := range tests {
		if got := DefaultFromURL(tt.url); got != tt.want {
			t.Errorf("DefaultFromURL(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestDefaultFromURL_UnsafeRaw(t *testing.T) {
	// defaultFromURLUnsafe 保留原始 wget 语义：? 不会被替换
	got := defaultFromURLUnsafe("https://e.com/xx/?a=v")
	if want := "index.html?a=v"; got != want {
		t.Errorf("defaultFromURLUnsafe = %q, want %q", got, want)
	}
}

// TestDefaultFromURLThenSafe 验证"生成 + 清理"的完整链路（与 server 端一致）。
// DefaultFromURL 已内置 Safe，双重包装退化为单层 Safe，结果不变。
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
