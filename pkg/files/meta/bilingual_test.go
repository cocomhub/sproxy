// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"testing"
)

// TestValidateExtra_Bilingual 验证中日双语字段（actress_cn/genres_cn）与
// 补充字段（has_en_sub/cover_url）被白名单接受——sdserver dump-meta 输出的
// 完整 MetaEntry 必须能通过（title_ja/title_cn 已支持，现补 cn 版女优/类型）。
func TestValidateExtra_Bilingual(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		// A 档：内容（中日并存）
		"code":         "EXAMPLE-001",
		"maker":        "STUDIO X",
		"maker_slug":   "studio-x",
		"label":        "LABEL A",
		"series":       "シリーズ名",
		"actress":      []string{"東城えみ"},
		"actress_cn":   []string{"东城えみ"},
		"genres":       []string{"羞恥", "野外・露出"},
		"genres_cn":    []string{"羞耻", "野外露出"},
		"director":     "たっつん中野",
		"release_date": "2026-09-17",
		"title_ja":     "NHDTC-254 就寝中の夜行バス",
		"title_cn":     "NHDTC-254 五个胆小的女孩在夜间巴士上",
		"page_url":     "https://example.com/ja/EXAMPLE-001",
		"site":         "njavtv",
		// C 档：补充
		"duration_sec": int64(13546),
		"uncensored":   false,
		"has_cn_sub":   false,
		"has_en_sub":   false,
		"cover_url":    "https://example.com/cover.jpg",
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("双语完整 MetaEntry 应通过白名单: %v", err)
	}
}

// TestValidateExtra_Bilingual_MaxLen 验证 cn 版字段长度上限与 ja 同规格。
func TestValidateExtra_Bilingual_MaxLen(t *testing.T) {
	t.Parallel()
	long := make([]string, 0, 2)
	for range 2 {
		long = append(long, repeat("あ", 200))
	}
	if err := ValidateExtra(map[string]any{"actress_cn": long}); err == nil {
		t.Fatal("actress_cn 超 MaxLen 应拒绝")
	}
	if err := ValidateExtra(map[string]any{"genres_cn": long}); err == nil {
		t.Fatal("genres_cn 超 MaxLen 应拒绝")
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// TestTrimExtra_BilingualPreserved 验证 TrimExtra 不裁剪 cn 版非空字段。
func TestTrimExtra_BilingualPreserved(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"code":       "EXAMPLE-001",
		"actress":    []string{"東城えみ"},
		"actress_cn": []string{"东城えみ"},
		"genres_cn":  []string{"羞耻"},
		"title_cn":   "中文标题",
	}
	trimmed := TrimExtra(in)
	for _, k := range []string{"actress_cn", "genres_cn", "title_cn"} {
		if trimmed[k] == nil {
			t.Fatalf("TrimExtra 裁剪了 %s（cn 版非空字段应保留）", k)
		}
	}
}
