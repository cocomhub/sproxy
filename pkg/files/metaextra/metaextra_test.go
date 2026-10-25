// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metaextra

import (
	"strings"
	"testing"
)

// TestValidate_OK 验证合法字段全部通过（A/B/C 三档代表字段）。
func TestValidate_OK(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"code":             "NHDTC-254",
		"maker":            "NATURAL HIGH",
		"maker_slug":       "natural-high",
		"series":           "就寝中の夜行バス",
		"actress":          []string{"葵ぶるま"},
		"genres":           []string{"痴女", "巨乳"},
		"release_date":     "2026-09-17",
		"title_ja":         "NHDTC-254 就寝中の夜行バス",
		"page_url":         "https://njavtv.com/cn/nhdtc-254",
		"site":             "njavtv",
		"origin_name":      "NHDTA-003.avi",
		"source":           "pikpak_magnet",
		"pikpak_share_url": "https://mypikpak.com/s/abc",
		"pikpak_share_id":  "VP3Bz4uK5oC9h71F8p8swU0io2",
		"pikpak_hash":      "375467F3FED049B15C50C7E41AF35C3A5A4EEBEC",
		"mirror_urls":      []string{"https://keepshare.org/k/magnet:?xt=urn:btih:aa"},
		"duration_sec":     float64(8956),
		"resolution":       "720p",
		"uncensored":       true,
		"subtitle_files":   []string{"NHDTC-254.srt"},
	}
	if err := Validate(extra); err != nil {
		t.Fatalf("合法字段应通过: %v", err)
	}
}

// TestValidate_UnknownField 验证白名单外字段被拒（防任意键注入）。
func TestValidate_UnknownField(t *testing.T) {
	t.Parallel()
	err := Validate(map[string]any{"evil_key": "x"})
	if err == nil || !strings.Contains(err.Error(), "未知字段") {
		t.Fatalf("未知字段应拒绝, err=%v", err)
	}
}

// TestValidate_TooLong 验证超长字符串被拒（单字段上限）。
func TestValidate_TooLong(t *testing.T) {
	t.Parallel()
	err := Validate(map[string]any{"code": strings.Repeat("x", 100)})
	if err == nil || !strings.Contains(err.Error(), "超长") {
		t.Fatalf("超长应拒绝, err=%v", err)
	}
}

// TestValidate_TooLarge 验证总大小超 16 KiB 被拒（防滥用）。
func TestValidate_TooLarge(t *testing.T) {
	t.Parallel()
	err := Validate(map[string]any{"origin_name": strings.Repeat("y", 300)})
	if err != nil {
		t.Fatalf("单字段 300B 应通过: %v", err)
	}
	// 构造超大：多条 500B 数组元素
	big := make([]string, 40)
	for i := range big {
		big[i] = strings.Repeat("z", 500)
	}
	err = Validate(map[string]any{"mirror_urls": big})
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
		if err := Validate(c); err == nil {
			t.Fatalf("类型错误应拒绝: %v", c)
		}
	}
}

// TestValidate_Enum 验证枚举字段（site/source/resolution）非法值被拒。
func TestValidate_Enum(t *testing.T) {
	t.Parallel()
	for _, c := range []map[string]any{
		{"site": "unknown-site"},
		{"source": "torrent"},
		{"resolution": "2k"},
	} {
		if err := Validate(c); err == nil {
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
	if err := Validate(extra); err != nil {
		t.Fatalf("JSON 形态应通过: %v", err)
	}
}

// TestTrim 验证空白字符串被剔除（不污染 meta）。
func TestTrim(t *testing.T) {
	t.Parallel()
	got := Trim(map[string]any{"code": "  NHDTC-254  ", "maker": "   ", "actress": []string{"x"}})
	if got["code"] != "NHDTC-254" {
		t.Fatalf("code 应 trim: %v", got["code"])
	}
	if _, ok := got["maker"]; ok {
		t.Fatalf("空白字符串应剔除")
	}
	if _, ok := got["actress"]; !ok {
		t.Fatalf("非字符串字段应保留")
	}
}
