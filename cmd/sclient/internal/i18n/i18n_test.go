// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package i18n

import (
	"strings"
	"testing"
)

// 本包测试直接替换包级 lookupEnv seam（不修改真实进程环境）。
// 各用例修改同一包级变量，不能并发，故统一带 // sproxy:serial: 标记
// （R18 显式登记豁免，无需进入 serial_budgets.tsv）。

// setEnv 用固定 map 替换 lookupEnv，并在用例结束时恢复。
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	old := lookupEnv
	lookupEnv = func(key string) string {
		if v, ok := env[key]; ok {
			return v
		}
		return ""
	}
	t.Cleanup(func() { lookupEnv = old })
}

// TestLocale_Priority 钉住探测优先级：LC_ALL > LC_MESSAGES > LANG（空跳过）。
func TestLocale_Priority(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	cases := []struct {
		name                    string
		lcAll, lcMessages, lang string
		want                    string
	}{
		{"lc_all_wins", "en_US.UTF-8", "zh_CN", "zh_CN", "en"},
		{"lc_messages_second", "", "en_US.UTF-8", "zh_CN", "en"},
		{"lang_last", "", "", "en_US.UTF-8", "en"},
		{"all_empty_defaults_zh", "", "", "", "zh-CN"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, map[string]string{
				"LC_ALL":      tt.lcAll,
				"LC_MESSAGES": tt.lcMessages,
				"LANG":        tt.lang,
			})
			if got := Locale(); got != tt.want {
				t.Errorf("Locale() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLocale_zhVariants：zh 前缀（含变体）一律归 zh-CN。
func TestLocale_zhVariants(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	for _, loc := range []string{"zh", "zh_CN", "zh_CN.UTF-8", "zh-CN", "zh_TW", "zh-Hans-CN"} {
		t.Run(loc, func(t *testing.T) {
			setEnv(t, map[string]string{"LC_ALL": loc})
			if got := Locale(); got != "zh-CN" {
				t.Errorf("Locale(%q) = %q, want zh-CN", loc, got)
			}
		})
	}
}

// TestLocale_cLocales：C/POSIX 语义（含 codeset 后缀）归默认 zh-CN。
func TestLocale_cLocales(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	for _, loc := range []string{"C", "POSIX", "C.UTF-8", "c"} {
		t.Run(loc, func(t *testing.T) {
			setEnv(t, map[string]string{"LC_ALL": loc})
			if got := Locale(); got != "zh-CN" {
				t.Errorf("Locale(%q) = %q, want zh-CN", loc, got)
			}
		})
	}
}

// TestLocale_enVariants：en 前缀归 en。
func TestLocale_enVariants(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	for _, loc := range []string{"en", "en_US", "en_US.UTF-8", "en-GB"} {
		t.Run(loc, func(t *testing.T) {
			setEnv(t, map[string]string{"LANG": loc})
			if got := Locale(); got != "en" {
				t.Errorf("Locale(%q) = %q, want en", loc, got)
			}
		})
	}
}

// TestLocale_otherLangs：合法但非 zh/en 的语言代码回退 en（国际通用）。
func TestLocale_otherLangs(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	for _, loc := range []string{"fr", "fr_FR", "ja_JP", "de_DE.UTF-8", "es-ES"} {
		t.Run(loc, func(t *testing.T) {
			setEnv(t, map[string]string{"LANG": loc})
			if got := Locale(); got != "en" {
				t.Errorf("Locale(%q) = %q, want en", loc, got)
			}
		})
	}
}

// TestLocale_invalid：非法 locale 回退默认 zh-CN。
func TestLocale_invalid(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	for _, loc := range []string{"foo", "123", "_x", "!!!"} {
		t.Run(loc, func(t *testing.T) {
			setEnv(t, map[string]string{"LC_ALL": loc})
			if got := Locale(); got != "zh-CN" {
				t.Errorf("Locale(%q) = %q, want zh-CN", loc, got)
			}
		})
	}
}

// TestDict_KeySetsEqual 门禁式防漏译：en 表与 zh 表键集合必须完全相等。
// 变异验证：删 en 表任意键 → 本测试红。
func TestDict_KeySetsEqual(t *testing.T) {
	zh, ok := dict["zh-CN"]
	if !ok {
		t.Fatalf("dict 缺 zh-CN 表")
	}
	en, ok := dict["en"]
	if !ok {
		t.Fatalf("dict 缺 en 表")
	}
	for key := range zh {
		if _, ok := en[key]; !ok {
			t.Errorf("en 表缺键 %q（zh 表有）——补 en 翻译", key)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Errorf("zh 表缺键 %q（en 表有）——两表键集必须一致", key)
		}
	}
	if len(zh) == 0 {
		t.Fatalf("字典为空：门禁失效（至少 1 键才有效）")
	}
}

// TestT_MissingKeyReturnsKey：缺失键回退原文（fail-open，绝不空串）。
func TestT_MissingKeyReturnsKey(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{"LANG": "en_US.UTF-8"})
	key := "不存在的键 abc"
	if got := T(key); got != key {
		t.Errorf("T(缺失键) = %q, want 原键 %q", got, key)
	}
}

// TestT_DefaultChinese：默认环境（无语言变量）输出原中文。
func TestT_DefaultChinese(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{})
	if got := T("暂无分享链接"); got != "暂无分享链接" {
		t.Errorf("T(默认) = %q, want 原中文", got)
	}
}

// TestT_English：en 下取英文翻译。
func TestT_English(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{"LANG": "en_US.UTF-8"})
	if got := T("暂无分享链接"); got != "No share links" {
		t.Errorf("T(en) = %q, want %q", got, "No share links")
	}
}

// TestF_Placeholders：F 翻译并填充 fmt 占位符（默认中文）。
func TestF_Placeholders(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{})
	got := F("分享链接: %s", "https://example.com/s/tok")
	if want := "分享链接: https://example.com/s/tok"; got != want {
		t.Errorf("F(zh) = %q, want %q", got, want)
	}
}

// TestF_EnglishPlaceholders：en 下翻译与填充同时生效。
func TestF_EnglishPlaceholders(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{"LANG": "en_US.UTF-8"})
	got := F("分享链接: %s", "https://example.com/s/tok")
	if want := "Share link: https://example.com/s/tok"; got != want {
		t.Errorf("F(en) = %q, want %q", got, want)
	}
}

// TestF_EscapedPercent：格式串内 %% 经 F 后归一为 %（与原 Fprintf 行为一致）。
func TestF_EscapedPercent(t *testing.T) {
	// sproxy:serial: 修改包级 lookupEnv seam，与其它探测用例互斥
	setEnv(t, map[string]string{})
	got := F("  磁盘分区: %s / %s (%.1f%%)", "1.0 MB", "2.0 MB", 50.0)
	if want := "  磁盘分区: 1.0 MB / 2.0 MB (50.0%)"; got != want {
		t.Errorf("F(%%转义) = %q, want %q", got, want)
	}
	if strings.Contains(got, "%%") {
		t.Errorf("F 未归一 %%: %q", got)
	}
}
