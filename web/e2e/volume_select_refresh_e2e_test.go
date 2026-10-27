// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volume_select_refresh_e2e_test.go — 「卷」下拉（#volume-select，单一卷上下文）真浏览器 E2E。
//
// 历史缺陷与本次收敛（用户裁定 2026-10-27）：
//  1. **登录成功后派生数据必须刷新**（2026-10-21 修复）：下拉只在 DOMContentLoaded 初始化一次，
//     登录前 `GET /api/volumes` 401 ⇒ 下拉只有「全部卷 / auto」；登录成功后若不重拉，运行时
//     创建的卷（secretdata `vault` 等）永远看不到。本文件锁住该行为。
//  2. **两个下拉合并为一个**（本次）：原「过滤卷」`#volume-filter` 与「上传卷」`#upload-volume`
//     各自独立，导致「选了 baidu 卷进目录 → 刷新后卷上下文丢失、目录还在 ⇒ 显示空列表」。
//     收敛为单一 `#volume-select`：同时决定 ① 列表过滤 ② 上传目标 ③ 卷内操作回退卷。
//  3. **选择持久化 + 按卷记忆最新路径**（本次，用户明示）：
//     - 选卷写 localStorage `sproxy_volume`，刷新/重开页面后保留；
//     - 每个卷独立记忆最后访问目录（`sproxy_subdir_vol_<卷>`），切卷自动跳到该卷上次目录；
//       无卷（全部卷 / auto）用全局键 `sproxy_subdir` 兜底。
//
// 断言策略：拦截 `/api/volumes` 保证「登录前 401 / 登录后放行」确定性；其余只断言用户可见
// 的 DOM 与 localStorage 结果。

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestVolumes_LoginRefreshesSelect 登录成功后「卷」下拉必须重新拉取并渲染（含运行时卷）。
func TestVolumes_LoginRefreshesSelect(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.Registration.ForceTOTP = true // 走 TOTP 登录分支（无 sk）
	})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// allow=false 期间所有 /api/volumes 返回 401（模拟「登录前无凭据」）。
	var allow atomic.Bool
	var volumesHits atomic.Int64
	if err := page.Route("**/api/volumes", func(route playwright.Route) {
		volumesHits.Add(1)
		if !allow.Load() {
			_ = route.Fulfill(playwright.RouteFulfillOptions{
				Status:  playwright.Int(http.StatusUnauthorized),
				Body:    playwright.String(`{"error":"unauthorized"}`),
				Headers: map[string]string{"Content-Type": "application/json"},
			})
			return
		}
		_ = route.Continue()
	}); err != nil {
		t.Fatalf("Route 拦截 /api/volumes: %v", err)
	}

	if _, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); gerr != nil {
		t.Fatalf("导航: %v", gerr)
	}
	if hits := volumesHits.Load(); hits == 0 {
		t.Fatal("页面加载时未请求 /api/volumes（用例前提不成立：初始化路径已改？）")
	}
	// 前提：登录前「卷」下拉只有「全部卷 / auto」（没有 default 卷 option）。
	if err := waitLoc(page, "#volume-select option[value='default']", playwright.WaitForSelectorStateAttached, 1200); err == nil {
		t.Fatal("登录前 /api/volumes 401 却已填充 default ⇒ 用例前提不成立（拦截未生效）")
	}

	// 真实注册 + TOTP 登录（凭据生效：后续请求改走隧道，故只断言 DOM 结果）。
	openLoginModal(t, page, "")
	reg := registerViaUI(t, page, "e2e-vol-refresh")
	fillTOTPLogin(t, page, reg.AK, reg.Base32Secret)
	allow.Store(true)
	// 登录点击期间必须出现**新的**事件流请求（同步等待请求事件，无 Go 侧固定等待）。
	if _, eerr := page.ExpectRequest("**/api/events*", func() error {
		return page.Locator("#do-login-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(15000)}); eerr != nil {
		t.Fatalf("登录后未重新发起事件流连接 ⇒ 实时更新在登录后仍失效: %v", eerr)
	}

	// 缺陷点：旧实现登录后只 refreshList() ⇒ 下拉保持空（运行时卷不可见）。
	if werr := waitLoc(page, "#volume-select option[value='default']", playwright.WaitForSelectorStateAttached, 10000); werr != nil {
		t.Fatalf("登录后「卷」下拉未刷新（缺 default option）⇒ 运行时创建的卷不可见: %v", werr)
	}

	// 下拉接线成立：选择该卷 → DOM 值与 localStorage 同步（刷新后保留的前提）。
	if _, serr := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"default"}}); serr != nil {
		t.Fatalf("选择卷: %v", serr)
	}
	if cur, _ := page.Evaluate("document.getElementById('volume-select').value"); cur != "default" {
		t.Fatalf("卷选择值 = %v, want default", cur)
	}
	stored, _ := page.Evaluate("localStorage.getItem('sproxy_volume')")
	if stored != "default" {
		t.Fatalf("卷选择未持久化到 localStorage（sproxy_volume=%v）⇒ 刷新后会丢失", stored)
	}
}

// TestVolumes_SelectPersistsAcrossReload 选卷 → 刷新页面 → 卷仍选中且列表按该卷请求。
//
// 缺陷（用户实测 2026-10-27）：「选 baidu 卷进入某目录后刷新页面，卷信息丢失但目录正确 ⇒
// 列表显示为空」——因为卷上下文只存内存（_volumeFilter 初值恒 ”），刷新归零后列表按
// 全部卷请求而 currentSubdir 仍是卷内子目录 ⇒ 空列表。本用例锁住修复。
func TestVolumes_SelectPersistsAcrossReload(t *testing.T) {
	baseURL, roots, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	// disk2 卷内放一个文件（选卷后列表应只见该卷内容）。
	volFile(t, roots, "disk2", "only-on-disk2.txt", "d2")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, "#volume-select option[value='disk2']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("卷下拉未填充 disk2: %v", err)
	}
	if _, err := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"disk2"}}); err != nil {
		t.Fatalf("选择 disk2: %v", err)
	}
	waitTextVisible(t, page, "#file-list", "only-on-disk2.txt", 8000)

	// 刷新：卷选择必须保留（localStorage 恢复），列表仍只见 disk2 内容。
	if _, err := page.Reload(playwright.PageReloadOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("刷新页面: %v", err)
	}
	if err := waitLoc(page, "#volume-select option[value='disk2']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("刷新后卷下拉未恢复: %v", err)
	}
	// WaitForFunction 轮询（无 time.Sleep，符合 R14 棘轮）：下拉值恢复为 disk2。
	if _, err := page.WaitForFunction(
		"() => { const el = document.getElementById('volume-select'); return !!el && el.value === 'disk2'; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		cur, _ := page.Evaluate("document.getElementById('volume-select').value")
		t.Fatalf("刷新后卷选择 = %v, want disk2（选择未持久化/未恢复）: %v", cur, err)
	}
	waitTextVisible(t, page, "#file-list", "only-on-disk2.txt", 8000)
}

// TestVolumes_PerVolumeSubdirMemory 各卷独立记忆最新目录（用户明示 2026-10-27）。
//
// 场景：disk2 卷进入 sub/ → 切到 main 卷（应回卷根，而非停留在 sub/）→ main 进 other/ →
// 切回 disk2（应自动回到 sub/，而非 other/ 或根）。
func TestVolumes_PerVolumeSubdirMemory(t *testing.T) {
	baseURL, roots, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	volFile(t, roots, "disk2", "sub/in-sub.txt", "s")
	volFile(t, roots, "main", "other/in-other.txt", "o")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, "#volume-select option[value='disk2']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("卷下拉未填充: %v", err)
	}

	// ① disk2 → 进 sub/。
	if _, err := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"disk2"}}); err != nil {
		t.Fatalf("选择 disk2: %v", err)
	}
	navigateSubdirInUI(t, page, "sub")
	assertCurrentSubdir(t, page, "sub")

	// ② 切到 main：该卷无目录记录 ⇒ 回落卷根（不能停留在 disk2 的 sub/）。
	if _, err := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"main"}}); err != nil {
		t.Fatalf("选择 main: %v", err)
	}
	assertCurrentSubdir(t, page, "")

	// ③ main → 进 other/。
	navigateSubdirInUI(t, page, "other")
	assertCurrentSubdir(t, page, "other")

	// ④ 切回 disk2：必须自动恢复到该卷上次所在 sub/（核心断言）。
	if _, err := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"disk2"}}); err != nil {
		t.Fatalf("切回 disk2: %v", err)
	}
	assertCurrentSubdir(t, page, "sub")
	waitTextVisible(t, page, "#file-list", "in-sub.txt", 8000)
}

// navigateSubdirInUI 在 UI 中进入指定子目录：点该目录行的「进入」按钮。
func navigateSubdirInUI(t *testing.T, page playwright.Page, dir string) {
	t.Helper()
	if err := waitLoc(page, ".dir-enter-btn[data-subdir='"+dir+"']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("目录 %q 的进入按钮未渲染: %v", dir, err)
	}
	if err := page.Locator(".dir-enter-btn[data-subdir='" + dir + "']").Click(); err != nil {
		t.Fatalf("点击进入 %q: %v", dir, err)
	}
}

// assertCurrentSubdir 等待 currentSubdir（浏览器全局）等于 want（WaitForFunction，无 sleep）。
func assertCurrentSubdir(t *testing.T, page playwright.Page, want string) {
	t.Helper()
	expr := "() => (typeof currentSubdir !== 'undefined') && currentSubdir === " + jsQuote(want)
	if _, err := page.WaitForFunction(expr, nil, playwright.PageWaitForFunctionOptions{
		Timeout: playwright.Float(8000),
	}); err != nil {
		got, _ := page.Evaluate("typeof currentSubdir !== 'undefined' ? currentSubdir : '<undefined>'")
		t.Fatalf("currentSubdir = %v, want %q: %v", got, want, err)
	}
}

// TestVolumesPanel_RefreshButtonTriggersAPI 卷面板「重建索引」按钮（S5）：
// 打开卷面板 → 点某卷行的 vol-refresh-btn → 断言发出 POST /api/volumes/refresh?volume=<卷>
// 且 toast 成功（幂等：本地卷也成功）。
func TestVolumesPanel_RefreshButtonTriggersAPI(t *testing.T) {
	// sproxy:serial: 面板用例共享 #volumes-panel 交互路径，串行更稳（与其它面板用例同一时序域）。
	baseURL, _, cleanup := testServerAdmin(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 打开监控弹窗 → 卷 tab（复用既有面板入口流程）。
	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("导航: %v", err)
	}
	if err := page.Locator("#stats-btn").Click(); err != nil {
		t.Fatalf("打开监控: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("监控弹窗未显示: %v", err)
	}
	if err := page.Locator("#volumes-tab").Click(); err != nil {
		t.Fatalf("切到卷 tab: %v", err)
	}
	if err := waitLoc(page, "#volumes-panel .vol-refresh-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("卷面板未渲染「重建索引」按钮（S5 接线缺失）: %v", err)
	}

	// 点第一行的重建索引 → 应发出 POST /api/volumes/refresh?volume=<卷>。
	acceptDialog(page, "") // 有 confirm（防误点）
	req, err := page.ExpectRequest("**/api/volumes/refresh?*", func() error {
		return page.Locator("#volumes-panel .vol-refresh-btn").First().Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/volumes/refresh（按钮未接线？）: %v", err)
	}
	if !strings.Contains(req.URL(), "volume=") {
		t.Errorf("refresh URL = %q，应带 volume 参数", req.URL())
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("refresh method = %q，应 POST", got)
	}
}
