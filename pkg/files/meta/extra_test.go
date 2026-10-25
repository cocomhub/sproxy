// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"strings"
	"testing"
)

// TestValidate_OK 验证合法字段全部通过（A/B/C 三档代表字段）。
func TestValidate_OK(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"code":           "MOVIE-001",
		"maker":          "STUDIO X",
		"maker_slug":     "studio-x",
		"series":         "夜行バス",
		"actress":        []string{"葵ぶるま"},
		"genres":         []string{"ドラマ"},
		"release_date":   "2026-09-17",
		"title_ja":       "MOVIE-001 夜行バス",
		"page_url":       "https://example.com/ja/movie-001",
		"site":           "example",
		"origin_name":    "MOVIE-001.avi",
		"source":         "cloud_share",
		"share_url":      "https://example.com/s/abc",
		"share_id":       "sh-12345",
		"share_hash":     "AABBCCDD11223344556677889900",
		"mirror_urls":    []string{"https://mirror.example.com/a"},
		"duration_sec":   float64(8956),
		"resolution":     "720p",
		"uncensored":     true,
		"subtitle_files": []string{"MOVIE-001.srt"},
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("合法字段应通过: %v", err)
	}
}

// TestValidate_UnknownField 验证白名单外字段被拒（防任意键注入）。
func TestValidate_UnknownField(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"evil_key": "x"})
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("未知字段应拒绝, err=%v", err)
	}
}

// TestValidate_TooLong 验证超长字符串被拒（单字段上限）。
func TestValidate_TooLong(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"code": strings.Repeat("x", 100)})
	if err == nil || !strings.Contains(err.Error(), "超长") {
		t.Fatalf("超长应拒绝, err=%v", err)
	}
}

// TestValidate_TooLarge 验证总大小超 16 KiB 被拒（防滥用）。
func TestValidate_TooLarge(t *testing.T) {
	t.Parallel()
	err := ValidateExtra(map[string]any{"origin_name": strings.Repeat("y", 300)})
	if err != nil {
		t.Fatalf("单字段 300B 应通过: %v", err)
	}
	// 构造超大：多条 500B 数组元素
	big := make([]string, 40)
	for i := range big {
		big[i] = strings.Repeat("z", 500)
	}
	err = ValidateExtra(map[string]any{"mirror_urls": big})
	if err == nil || !strings.Contains(err.Error(), "超上限") {
		t.Fatalf("总体超限应拒绝, err=%v", err)
	}
}

// TestValidate_WrongType 验证类型不匹配被拒。
func TestValidate_WrongType(t *testing.T) {
	t.Parallel()
	cases := []map[string]any{
		{"code": 123},
		{"actress": "not-array"},
		{"uncensored": "true"},
		{"duration_sec": "8956"},
	}
	for _, c := range cases {
		if err := ValidateExtra(c); err == nil {
			t.Fatalf("类型错误应拒绝: %v", c)
		}
	}
}

// TestValidate_Enum 验证枚举字段（source/resolution）非法值被拒。
func TestValidate_Enum(t *testing.T) {
	t.Parallel()
	for _, c := range []map[string]any{
		{"source": "torrent"},
		{"resolution": "2k"},
	} {
		if err := ValidateExtra(c); err == nil {
			t.Fatalf("枚举外取值应拒绝: %v", c)
		}
	}
}

// TestValidate_JSONDecoded 验证 JSON 反序列化形态（[]any / float64）被接受。
func TestValidate_JSONDecoded(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"actress":      []any{"葵ぶるま", "椿まひる"},
		"duration_sec": float64(8956),
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("JSON 形态应通过: %v", err)
	}
}

// TestTrim 验证空白字符串被剔除（不污染 meta）。
func TestTrim(t *testing.T) {
	t.Parallel()
	got := TrimExtra(map[string]any{"code": "  MOVIE-001  ", "maker": "   ", "actress": []string{"x"}})
	if got["code"] != "MOVIE-001" {
		t.Fatalf("code 应 trim: %v", got["code"])
	}
	if _, ok := got["maker"]; ok {
		t.Fatalf("空白字符串应剔除")
	}
	if _, ok := got["actress"]; !ok {
		t.Fatalf("非字符串字段应保留")
	}
}

// TestValidateExtra_CustomPrefix 验证 `x_` 前缀自定义扩展字段：
// 白名单外键可携带（任意语义），但类型/键名/总量仍受限。
func TestValidateExtra_CustomPrefix(t *testing.T) {
	t.Parallel()
	// 合法：x_ 自定义字段
	if err := ValidateExtra(map[string]any{"code": "EXAMPLE-001", "x_my_site_meta": "abc"}); err != nil {
		t.Fatalf("x_ 字段应通过: %v", err)
	}
	// 非法：非 x_ 前缀的白名单外键 → 拒绝
	if err := ValidateExtra(map[string]any{"random_key": 1}); err == nil {
		t.Fatal("白名单外非 x_ 键应拒绝")
	}
	// 非法：x_ 键名含大写 → 拒绝
	if err := ValidateExtra(map[string]any{"x_MyKey": 1}); err == nil {
		t.Fatal("x_ 键名大写应拒绝")
	}
	// 非法：x_ 值类型不支持（struct）→ 拒绝
	if err := ValidateExtra(map[string]any{"x_bad": struct{ A int }{1}}); err == nil {
		t.Fatal("x_ 值不支持 struct 应拒绝")
	}
	// 合法：x_ 布尔/数字/[]string
	if err := ValidateExtra(map[string]any{"x_enabled": true, "x_count": float64(3), "x_tags": []string{"a"}}); err != nil {
		t.Fatalf("x_ 标量应通过: %v", err)
	}
}

// TestTrimExtra_CustomPreserved 验证 TrimExtra 保留 x_ 字段（不被当空值裁剪）。
func TestTrimExtra_CustomPreserved(t *testing.T) {
	t.Parallel()
	in := map[string]any{"code": "X-1", "x_foo": "", "x_bar": "keep", "site": ""}
	out := TrimExtra(in)
	if _, ok := out["x_foo"]; ok {
		t.Fatal("x_ 空字符串应裁剪（与白名单同语义）")
	}
	if out["x_bar"] != "keep" {
		t.Fatalf("x_bar = %v, want keep", out["x_bar"])
	}
	if _, ok := out["site"]; ok {
		t.Fatal("白名单空值也应裁剪")
	}
}
