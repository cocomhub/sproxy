// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2_shell_e2e_test.go — P0 壳层冒烟：路由 / 主题 / 深链刷新 / 旧版入口。
package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestV2Shell_RouteThemeAndEntry 验证 /ui/v2/ 可用且不破坏旧版入口。
func TestV2Shell_RouteThemeAndEntry(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 播种一个真实目录，验证 P1a 列表与面包屑（测试实例允许 loopback 匿名）。
	resp, mkErr := http.Post(baseURL+"/mkdir?dirname=v2demo", "", nil)
	if mkErr != nil {
		t.Fatalf("seed mkdir: %v", mkErr)
	}
	resp.Body.Close()

	if _, err := page.Goto(baseURL + "/ui/v2/"); err != nil {
		t.Fatalf("打开 /ui/v2/: %v", err)
	}
	if err := waitLoc(page, ".nav[data-route]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("v2 侧栏未渲染: %v", err)
	}
	if n, _ := page.Locator(".nav[data-route]").Count(); n != 4 {
		t.Fatalf("侧栏入口数 = %d，期望 4", n)
	}
	if txt, _ := page.Locator("#v2-title").TextContent(); txt != "文件" {
		t.Fatalf("默认路由标题 = %q，期望 文件", txt)
	}

	// P1a：列表来自 /api/files，且可进入子目录。
	if err := waitLoc(page, "#v2-file-body tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("文件列表未渲染: %v", err)
	}
	if txt, _ := page.Locator("#v2-file-body").TextContent(); !strings.Contains(txt, "v2demo") {
		t.Fatalf("列表缺少播种目录 v2demo: %q", txt)
	}
	if err := page.Locator("#v2-file-body a").First().Click(); err != nil {
		t.Fatalf("进入 v2demo: %v", err)
	}
	if h, _ := page.Evaluate("() => location.hash", nil); h != "#/files/v2demo" {
		t.Fatalf("子目录 hash = %v", h)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-crumbs').textContent.indexOf('v2demo') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("面包屑未更新为 v2demo: %v", err)
	}

	// P1b：UI 新建目录 → 选择 → 批量删除（走真实 /mkdir 与 /rmdir）。
	if err := page.Locator("#v2-mkdir").Click(); err != nil {
		t.Fatalf("打开新建目录: %v", err)
	}
	if err := page.Locator("#v2-dlg-input").Fill("uifolder"); err != nil {
		t.Fatalf("填写目录名: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("提交新建目录: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-file-body').textContent.indexOf('uifolder') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("新建目录未出现在列表: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=uifolder] .cb").Check(); err != nil {
		t.Fatalf("勾选新目录: %v", err)
	}
	if err := page.Locator("#v2-batch-delete").Click(); err != nil {
		t.Fatalf("点批量删除: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("确认删除: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-file-body').textContent.indexOf('uifolder') === -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("目录删除后仍在列表: %v", err)
	}

	// 切换路由：hash + 面板可见性。
	if err := page.Locator(".nav[data-route='transfer']").Click(); err != nil {
		t.Fatalf("点击传输: %v", err)
	}
	if h, _ := page.Evaluate("() => location.hash", nil); h != "#/transfer" {
		t.Fatalf("hash = %v，期望 #/transfer", h)
	}
	hidden, _ := page.Evaluate("() => document.querySelector('[data-panel=transfer]').hidden", nil)
	if hidden != false {
		t.Fatalf("传输面板应可见，实际 hidden=%v", hidden)
	}

	// 刷新后深链保持。
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新: %v", err)
	}
	if err := waitLoc(page, "[data-panel='transfer']", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("刷新后未停留在传输: %v", err)
	}

	// 主题切换。
	before, _ := page.Evaluate("() => document.documentElement.getAttribute('data-theme')", nil)
	if err := page.Locator("#v2-theme-toggle").Click(); err != nil {
		t.Fatalf("切换主题: %v", err)
	}
	after, _ := page.Evaluate("() => document.documentElement.getAttribute('data-theme')", nil)
	if before == after {
		t.Fatalf("主题未切换：%v -> %v", before, after)
	}

	// 旧版入口：旧页提供试用链接，且旧页本身仍可用。
	if _, err := page.Goto(baseURL + "/ui/"); err != nil {
		t.Fatalf("打开旧版: %v", err)
	}
	href, err := page.Locator("#v2-entry-link").GetAttribute("href")
	if err != nil || href != "/ui/v2/" {
		t.Fatalf("旧版试用入口 href = %v（err=%v）", href, err)
	}
}
