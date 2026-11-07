// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// force_refresh_e2e_test.go — 工具栏「强制刷新」（#refresh-btn）真浏览器 E2E。
//
// 背景（用户要求 2026-10-29）：卷状态缓存**默认永不过期**，所以「重新列举」不再能感知
// 进程外改动（本机直写盘 / 其它实例 / 网盘 App 直改）。Web UI 必须提供**明确可用**的强制
// 刷新：先 `POST /api/volumes/refresh`（当前卷；全部卷上下文则不带 volume），再重新列举。
//
// 断言策略：拦截 `/api/volumes/refresh` 与 `/api/files`，断言点按钮后 ① 发出 refresh 请求
// ② 紧接着重新请求文件列表 ③ 按当前卷上下文带 volume 参数。

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestForceRefresh_ButtonRefreshesCacheThenLists 钉住：点「强制刷新」⇒ 清服务端卷缓存 + 重新列举。
func TestForceRefresh_ButtonRefreshesCacheThenLists(t *testing.T) {
	// sproxy:serial: Playwright 浏览器 fixture 共享页面/路由拦截状态，串行更稳（与同目录用例同一时序域）。
	baseURL, _, cleanup := testServerCfg(t, nil)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	var refreshHits atomic.Int64
	var refreshQuery atomic.Value
	var fileHits atomic.Int64
	if err := page.Route("**/api/volumes/refresh**", func(route playwright.Route) {
		refreshHits.Add(1)
		refreshQuery.Store(route.Request().URL())
		_ = route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(http.StatusOK),
			Body:    playwright.String(`{"success":true,"message":"全部卷状态缓存已清空"}`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route /api/volumes/refresh: %v", err)
	}
	// 注意：不能用 `**/api/files*` —— 那会把 `/ui/sclient/api/files.js` 也拦成 JSON，
	// 导致页面脚本语法错误（`Unexpected token ':'`）且交互全失效。
	filesHandler := func(route playwright.Route) {
		fileHits.Add(1)
		_ = route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(http.StatusOK),
			Body:    playwright.String(`{"files":[],"total":0}`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}
	for _, pattern := range []string{"**/api/files", "**/api/files?**"} {
		if err := page.Route(pattern, filesHandler); err != nil {
			t.Fatalf("Route %s: %v", pattern, err)
		}
	}

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("导航: %v", err)
	}
	if err := waitLoc(page, "#refresh-btn", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("等待 #refresh-btn: %v", err)
	}
	before := fileHits.Load()

	if err := page.Locator("#refresh-btn").First().Click(playwright.LocatorClickOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("点击强制刷新: %v", err)
	}

	// refresh 必须恰好发出一次，且无 volume 参数（默认「全部卷」上下文）。
	if got := refreshHits.Load(); got != 1 {
		t.Fatalf("强制刷新应发 1 次 /api/volumes/refresh，got %d", got)
	}
	if raw, _ := refreshQuery.Load().(string); strings.Contains(raw, "volume=") {
		t.Fatalf("全部卷上下文不应带 volume 参数，got %s", raw)
	}
	// 清空缓存后必须**重新列举**（不是只弹 toast）——等成功 toast（在 refreshList 之后才出现）。
	if err := waitLoc(page, "#toast.show", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("等待强制刷新完成 toast: %v", err)
	}
	if got := fileHits.Load(); got <= before {
		t.Fatalf("强制刷新后应重新请求文件列表（before=%d, after=%d）", before, got)
	}
}

// TestForceRefresh_IncludesVolumeWhenSelected 钉住：选中具体卷时 refresh 带 volume 参数
// （只清该卷缓存，避免无谓地清空全部卷）。
func TestForceRefresh_IncludesVolumeWhenSelected(t *testing.T) {
	// sproxy:serial: 同上（Playwright 页面 fixture 非并发安全）。
	baseURL, _, cleanup := testServerCfg(t, nil)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	var refreshQuery atomic.Value
	if err := page.Route("**/api/volumes/refresh**", func(route playwright.Route) {
		refreshQuery.Store(route.Request().URL())
		_ = route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(http.StatusOK),
			Body:    playwright.String(`{"success":true,"message":"ok"}`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route refresh: %v", err)
	}
	filesHandler := func(route playwright.Route) {
		_ = route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(http.StatusOK),
			Body:    playwright.String(`{"files":[],"total":0}`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}
	for _, pattern := range []string{"**/api/files", "**/api/files?**"} {
		if err := page.Route(pattern, filesHandler); err != nil {
			t.Fatalf("Route %s: %v", pattern, err)
		}
	}

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("导航: %v", err)
	}
	if err := waitLoc(page, "#refresh-btn", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("等待 #refresh-btn: %v", err)
	}
	// 直接写入页面上下文（等价于用户在下拉里选中 default 卷）。
	if _, err := page.Evaluate(`() => { localStorage.setItem('sproxy_volume', 'default'); }`, nil); err != nil {
		t.Fatalf("设置卷上下文: %v", err)
	}
	if _, err := page.Reload(playwright.PageReloadOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("重载: %v", err)
	}
	if err := waitLoc(page, "#refresh-btn", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("等待重载后的 #refresh-btn: %v", err)
	}
	if err := page.Locator("#refresh-btn").First().Click(playwright.LocatorClickOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("点击强制刷新: %v", err)
	}
	if err := waitLoc(page, "#toast.show", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("等待刷新完成 toast: %v", err)
	}
	raw, _ := refreshQuery.Load().(string)
	if !strings.Contains(raw, "volume=default") {
		t.Fatalf("选中 default 卷时 refresh 应带 volume=default，got %q", raw)
	}
}
