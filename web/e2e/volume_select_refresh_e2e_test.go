// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volume_select_refresh_e2e_test.go — 「登录成功后派生数据必须刷新」真浏览器 E2E。
//
// 缺陷（用户实测，2026-10-21）：文件页「过滤卷」`#volume-filter` 与工具栏「上传卷」`#upload-volume`
// 只在 DOMContentLoaded 初始化一次。登录前那次 `GET /api/volumes` 因无凭据 401 ⇒ 下拉只剩
// 「全部卷」/auto；登录成功后旧实现只 `refreshList()`，下拉**永不重试** ⇒ **运行时创建的卷**
// （如 secretdata `vault`）在「过滤卷」里看不到（服务端数据是齐的，纯前端联动缺陷）。
//
// 同类问题一并覆盖：事件流（SSE）在 401/403 时**永久停止**（旧实现不再重连）⇒ 登录后实时更新
// 仍死。本用例断言登录后事件流重新建立。
//
// 断言策略：登录前把 `/api/volumes` 确定性判为 401（`page.Route` 拦截，直连模式生效）；
// 登录后凭据生效、请求改走**隧道**（外层 `/tunnel`），故**不断言 URL**，只断言 DOM 结果
// （两个下拉被填充）——这才是用户可见的行为。

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestVolumes_LoginRefreshesSelects 登录成功后两个卷下拉必须重新拉取并渲染（含运行时卷）。
func TestVolumes_LoginRefreshesSelects(t *testing.T) {
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
	// 前提：登录前两个下拉都只有「全部卷」/auto（没有 default 卷 option）。
	if err := waitLoc(page, "#upload-volume option[value='default']", playwright.WaitForSelectorStateAttached, 1200); err == nil {
		t.Fatal("登录前 /api/volumes 401 却已填充 default ⇒ 用例前提不成立（拦截未生效）")
	}
	if err := waitLoc(page, "#volume-filter option[value='default']", playwright.WaitForSelectorStateAttached, 1200); err == nil {
		t.Fatal("登录前 /api/volumes 401 却已填充过滤卷 default ⇒ 用例前提不成立")
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

	// 缺陷点：旧实现登录后只 refreshList() ⇒ 两个下拉保持空（运行时卷不可见）。
	if werr := waitLoc(page, "#upload-volume option[value='default']", playwright.WaitForSelectorStateAttached, 10000); werr != nil {
		t.Fatalf("登录后上传卷下拉未刷新（缺 default option）⇒ 登录后未重拉 /api/volumes: %v", werr)
	}
	if werr := waitLoc(page, "#volume-filter option[value='default']", playwright.WaitForSelectorStateAttached, 10000); werr != nil {
		t.Fatalf("登录后「过滤卷」下拉未刷新（缺 default option）⇒ 运行时创建的卷不可见: %v", werr)
	}

	// 下拉与列表接线仍成立：选择过滤卷 → 值生效（列表请求由既有接线负责）。
	if _, serr := page.Locator("#volume-filter").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"default"}}); serr != nil {
		t.Fatalf("选择过滤卷: %v", serr)
	}
	if cur, _ := page.Evaluate("document.getElementById('volume-filter').value"); cur != "default" {
		t.Fatalf("过滤卷选择值 = %v, want default", cur)
	}
}
