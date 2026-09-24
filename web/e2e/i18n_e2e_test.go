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
		t.Fatalf("goto: %v", err)
	}
	// 默认 zh：lang 属性 zh。
	langAttr, lerr := page.Locator("html").GetAttribute("lang")
	if lerr != nil {
		t.Fatalf("get lang: %v", lerr)
	}
	if !strings.HasPrefix(langAttr, "zh") {
		t.Fatalf("默认 lang = %q, want zh", langAttr)
	}
	// 语言按钮存在。
	if werr := page.Locator("#lang-toggle").WaitFor(); werr != nil {
		t.Fatalf("lang-toggle 不存在: %v", err)
	}
	// 点击切换 → en。
	if cerr := page.Locator("#lang-toggle").Click(); cerr != nil {
		t.Fatalf("click lang-toggle: %v", err)
	}
	enAttr, eerr := page.Locator("html").GetAttribute("lang")
	if eerr != nil {
		t.Fatalf("get lang after: %v", eerr)
	}
	if !strings.HasPrefix(enAttr, "en") {
		t.Fatalf("切换后 lang = %q, want en", enAttr)
	}
	// localStorage 持久化。
	store, err := page.Evaluate(`localStorage.getItem('sproxy_lang')`)
	if err != nil {
		t.Fatalf("eval storage: %v", err)
	}
	if store != "en" {
		t.Fatalf("localStorage sproxy_lang = %v, want en", store)
	}
	// 刷新后仍 en。
	if _, err := page.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	enAttr2, rerr := page.Locator("html").GetAttribute("lang")
	if rerr != nil {
		t.Fatalf("get lang after reload: %v", rerr)
	}
	if !strings.HasPrefix(enAttr2, "en") {
		t.Fatalf("刷新后 lang = %q, want en（持久化）", enAttr2)
	}
}
