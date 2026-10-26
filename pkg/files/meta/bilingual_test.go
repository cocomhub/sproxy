// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"strings"
	"testing"
)

// TestValidateExtra_AggregatedPlusXPrefix 验证多语言约定：
//   - 白名单字段（actress/genres）存**聚合值**（站点侧合并去重后的列表）；
//   - 各语言分离值走 `x_` 前缀（x_genres_cn / x_label_cn 等），服务端只做通用校验；
//   - sproxy 不理解业务语言语义（不新增 *_cn/*_ja 白名单字段，新增语种无需发版）。
func TestValidateExtra_AggregatedPlusXPrefix(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		// A 档：白名单字段存聚合值（ja 顺序优先 + cn 新增追加）
		"code":         "EXAMPLE-001",
		"maker":        "STUDIO X",
		"maker_slug":   "studio-x",
		"label":        "LABEL A",
		"series":       "シリーズ名",
		"actress":      []string{"東城えみ", "东城えみ"},
		"genres":       []string{"女子校生", "美少女", "中出し", "女高中生", "中出"},
		"director":     "たっつん中野",
		"release_date": "2026-09-17",
		"title_ja":     "EXAMPLE-001 就寝中の夜行バス",
		"page_url":     "https://example.com/ja/EXAMPLE-001",
		"site":         "example-site",
		// C 档
		"duration_sec": int64(13546),
		"uncensored":   false,
		"has_cn_sub":   false,
		// 语言分离值：x_ 前缀（客户端自由扩展，服务端不理解语义）
		"x_title_cn":    "EXAMPLE-001 中文标题",
		"x_genres_cn":   []string{"女子校生", "美少女"},
		"x_genres_ja":   []string{"女子校生", "美少女"},
		"x_actress_cn":  []string{"东城えみ"},
		"x_label_cn":    "标签 A",
		"x_series_cn":   "系列名",
		"x_director_cn": "导演名",
		"x_maker_cn":    "工作室 X",
		"x_has_en_sub":  false,
		"x_cover_url":   "https://example.com/cover.jpg",
	}
	if err := ValidateExtra(extra); err != nil {
		t.Fatalf("聚合值 + x_ 语言分离值应通过: %v", err)
	}
}

// TestValidateExtra_RejectsNonXLanguageKeys 验证**不带 x_ 前缀**的语言字段被拒
// （防止客户端误以为服务端新增了 actress_cn 白名单——约定必须走 x_）。
func TestValidateExtra_RejectsNonXLanguageKeys(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"actress_cn", "genres_cn", "label_cn", "series_cn", "genres_all"} {
		err := ValidateExtra(map[string]any{k: []string{"x"}})
		if err == nil {
			t.Fatalf("无 x_ 前缀的语言字段 %q 应被拒绝（约定走 x_）", k)
		}
		if !strings.Contains(err.Error(), CustomKeyPrefix) {
			t.Fatalf("错误提示应引导使用 %s 前缀: %v", CustomKeyPrefix, err)
		}
	}
}

// TestValidateExtra_XPrefixCharset 验证 x_ 键名只允许小写字母/数字/下划线（语种后缀自然合规）。
func TestValidateExtra_XPrefixCharset(t *testing.T) {
	t.Parallel()
	// 合法：小写语种后缀
	for _, k := range []string{"x_genres_cn", "x_genres_ja", "x_title_zh_hant", "x_label_en"} {
		if err := ValidateExtra(map[string]any{k: "v"}); err != nil {
			t.Fatalf("%q 应合法: %v", k, err)
		}
	}
	// 非法：大写 / 连字符
	for _, k := range []string{"x_Genres_cn", "x_genres-CN"} {
		if err := ValidateExtra(map[string]any{k: "v"}); err == nil {
			t.Fatalf("%q 应非法（仅小写字母数字下划线）", k)
		}
	}
}

// TestTrimExtra_XPrefixPreserved 验证 TrimExtra 保留 x_ 字段（不被当空值裁剪）。
func TestTrimExtra_XPrefixPreserved(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"code":        "EXAMPLE-001",
		"x_title_cn":  "中文标题",
		"x_genres_cn": []string{"中文类型"},
	}
	trimmed := TrimExtra(in)
	for _, k := range []string{"x_title_cn", "x_genres_cn"} {
		if trimmed[k] == nil {
			t.Fatalf("TrimExtra 不应裁剪 %s", k)
		}
	}
}
