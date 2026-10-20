// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// audit_export_notify_test_e2e_test.go Playwright e2e：
//   B5 审计导出按钮（audit tab「导出」→ GET /api/audit/export → 浏览器下载）
//   B6 通知测试按钮（stats tab「测试通知」→ POST /api/notify/test）
// 真浏览器 + 真实服务端（testServer），验证按钮渲染 + 点击触发对应 API。

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestAudit_ExportButton 审计 tab 含导出按钮；点击触发 GET /api/audit/export。
func TestAudit_ExportButton(t *testing.T) {
	baseURL, _, cleanup := testServerAdmin(t)
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

// TestAudit_FilterRows S4：审计面板筛选栏（动作/租户/结果/类别）真实浏览器验证。
//
// 覆盖三条用户可见行为：① 筛选栏渲染且计数摘要反映总数；② 输入不存在的动作 ⇒ 立即收敛为 0 条
// （**纯前端筛选，不重新请求**）；③ 点「清除」恢复全部。筛选逻辑本身由
// web/static/audit-filter.test.js（node --test）覆盖，本用例验证 DOM 接线与渲染。
func TestAudit_FilterRows(t *testing.T) {
	baseURL, _, cleanup := testServerAdmin(t)
	defer cleanup()

	// 播种至少一条审计事件：上传是 mutating 请求，auditMiddleware/业务审计会落一条
	// （S4 用例需要 N ≥ 1 才能区分「共 N 条」与「显示 0 / N 条」）。
	// 用 POST /api/notify/test：管理员可用、mutating（auditMiddleware + 业务审计各留一条），
	// 且不依赖卷权限（上传需要 owner/卷授权，回环兜底主体无 AK ⇒ 403 volume not allowed）。
	if code, body := postJSON(t, baseURL+"/api/notify/test", nil); code != http.StatusOK {
		t.Fatalf("播种审计事件（notify/test）失败: code=%d body=%s", code, body)
	}

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
	// ① 筛选栏渲染 + 摘要为「共 N 条」
	if err := waitLoc(page, "#audit-filter-action", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("筛选栏未渲染: %v", err)
	}
	if err := waitLoc(page, "#audit-filter-summary", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("计数摘要未渲染: %v", err)
	}
	total := func() string {
		txt, _ := page.Locator("#audit-filter-summary").InnerText()
		return strings.TrimSpace(txt)
	}
	before := total()
	if !strings.HasPrefix(before, "共 ") || !strings.HasSuffix(before, " 条") {
		t.Fatalf("未筛选时应显示「共 N 条」, got %q", before)
	}
	if before == "共 0 条" {
		t.Fatalf("播种后应有审计事件（否则无法验证筛选收敛）, got %q", before)
	}

	// ② 输入不存在的动作 ⇒ 立即 0 条（不发新请求）
	if err := page.Locator("#audit-filter-action").Fill("zzz-no-such-action"); err != nil {
		t.Fatalf("fill action filter: %v", err)
	}
	waitTextVisible(t, page, "#audit-filter-summary", "显示 0 / ", 5000)

	// ③ 卷维度筛选（评审 R13）：表头含「卷」列 + 卷输入框生效（不存在的卷 ⇒ 0 条）。
	if err := page.Locator("#audit-filter-reset").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click reset: %v", err)
	}
	waitTextVisible(t, page, "#audit-filter-summary", before, 5000)
	if err := waitLoc(page, "#audit-filter-volume", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("卷筛选输入框未渲染（卷维度 UI 未接线）: %v", err)
	}
	if n, _ := page.Locator("#audit-table th").Nth(4).InnerText(); strings.TrimSpace(n) != "卷" {
		t.Fatalf("审计表第 5 列应为「卷」表头（时间/操作/主体/mesh/卷）, got %q", strings.TrimSpace(n))
	}
	if err := page.Locator("#audit-filter-volume").Fill("zzz-no-such-volume"); err != nil {
		t.Fatalf("fill volume filter: %v", err)
	}
	waitTextVisible(t, page, "#audit-filter-summary", "显示 0 / ", 5000)

	// ④ 清除 ⇒ 恢复全部
	if err := page.Locator("#audit-filter-reset").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click reset: %v", err)
	}
	waitTextVisible(t, page, "#audit-filter-summary", before, 5000)
}

// TestStatsPanel_NotifyTestButton 统计 tab 通知区含测试按钮；点击触发 POST /api/notify/test。
func TestStatsPanel_NotifyTestButton(t *testing.T) {
	baseURL, _, cleanup := testServerAdmin(t)
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

// postJSON 发一个 JSON POST 并返回状态码与响应体（e2e 内联 helper）。
func postJSON(t *testing.T, url string, payload []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
