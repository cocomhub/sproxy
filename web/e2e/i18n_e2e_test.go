// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// i18n_e2e_test.go 是 WebUI 多语言（roadmap 11.10-H1）的 Playwright 真浏览器 e2e：
// 1. 默认 zh：页面标题/按钮为中文。
// 2. 点击语言切换按钮 → en：文案变英文 + localStorage 持久化。
// 3. 刷新后仍 en（持久化生效）。

import (
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestWebUI_I18N_LangToggle 语言切换 + 持久化（真浏览器）。
func TestWebUI_I18N_LangToggle(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
	defer pw.Stop()
	browser, err := pw.Chromium.Launch()
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer browser.Close()
	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	defer page.Close()

	if _, gerr := page.Goto(baseURL + "/ui/"); gerr != nil {
		t.Fatalf("goto: %v", gerr)
	}
	// 语言按钮存在（默认语言由 navigator/localStorage 决定，不锁具体值）。
	if werr := page.Locator("#lang-toggle").WaitFor(); werr != nil {
		t.Fatalf("lang-toggle 不存在: %v", werr)
	}
	// 记录初始语言（zh 或 en，由环境决定）。
	lang0, lerr := page.Locator("html").GetAttribute("lang")
	if lerr != nil {
		t.Fatalf("get initial lang: %v", lerr)
	}
	wantAfter := "zh"
	if strings.HasPrefix(lang0, "zh") {
		wantAfter = "en"
	}
	// 点击切换 → 语言翻转。
	if cerr := page.Locator("#lang-toggle").Click(); cerr != nil {
		t.Fatalf("click lang-toggle: %v", cerr)
	}
	lang1, eerr := page.Locator("html").GetAttribute("lang")
	if eerr != nil {
		t.Fatalf("get lang after: %v", eerr)
	}
	if !strings.HasPrefix(lang1, wantAfter) {
		t.Fatalf("切换后 lang = %q, want %s（初始 %s 翻转）", lang1, wantAfter, lang0)
	}
	// localStorage 持久化（与 html lang 一致）。
	store, serr := page.Evaluate(`localStorage.getItem('sproxy_lang')`)
	if serr != nil {
		t.Fatalf("eval storage: %v", serr)
	}
	if store != wantAfter {
		t.Fatalf("localStorage sproxy_lang = %v, want %s", store, wantAfter)
	}
	// 刷新后仍为翻转语言（持久化生效）。
	if _, rerr := page.Reload(); rerr != nil {
		t.Fatalf("reload: %v", rerr)
	}
	lang2, r2err := page.Locator("html").GetAttribute("lang")
	if r2err != nil {
		t.Fatalf("get lang after reload: %v", r2err)
	}
	if !strings.HasPrefix(lang2, wantAfter) {
		t.Fatalf("刷新后 lang = %q, want %s（持久化）", lang2, wantAfter)
	}
}
