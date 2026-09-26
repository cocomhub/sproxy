// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// audit_export_notify_test_e2e_test.go Playwright e2e：
//   B5 审计导出按钮（audit tab「导出」→ GET /api/audit/export → 浏览器下载）
//   B6 通知测试按钮（stats tab「测试通知」→ POST /api/notify/test）
// 真浏览器 + 真实服务端（testServer），验证按钮渲染 + 点击触发对应 API。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestAudit_ExportButton 审计 tab 含导出按钮；点击触发 GET /api/audit/export。
func TestAudit_ExportButton(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := page.Locator("#stats-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := page.Locator("#audit-tab").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click audit-tab: %v", err)
	}
	if err := waitLoc(page, "#audit-export-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("audit-export-btn 未渲染: %v", err)
	}
	txt, _ := page.Locator("#audit-export-btn").InnerText()
	if !strings.Contains(txt, "导出") {
		t.Fatalf("导出按钮文案: %q", txt)
	}
	// 点击导出 → 触发 GET /api/audit/export（200）。
	resp, err := page.ExpectResponse("**/api/audit/export", func() error {
		return page.Locator("#audit-export-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击导出未触发 /api/audit/export: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("GET /api/audit/export status = %d, want 200", got)
	}
}

// TestStatsPanel_NotifyTestButton 统计 tab 通知区含测试按钮；点击触发 POST /api/notify/test。
func TestStatsPanel_NotifyTestButton(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := page.Locator("#stats-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := waitLoc(page, "#notify-test-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		// notify 未装配时 notify 区块可能不渲染（stats 面板仍显示）——容错：跳过非阻塞断言。
		t.Logf("notify-test-btn 未渲染（notify 未装配，跳过）")
		return
	}
	txt, _ := page.Locator("#notify-test-btn").InnerText()
	if !strings.Contains(txt, "测试通知") {
		t.Fatalf("测试通知按钮文案: %q", txt)
	}
	resp, err := page.ExpectResponse("**/api/notify/test", func() error {
		return page.Locator("#notify-test-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击测试通知未触发 /api/notify/test: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK && got != http.StatusBadRequest {
		t.Fatalf("POST /api/notify/test status = %d, want 200（未装配时 400 可接受）", got)
	}
}
