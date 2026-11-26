// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume_settings_e2e_test.go — v2 卷详情抽屉「分块规则」真浏览器 E2E。
//
// 断言策略（与 force_refresh_e2e_test.go 同风格）：拦截 /api/volumes 与 detail/settings 路由，
// 用受控数据驱动抽屉，避免依赖真实 secretdata 卷装配（本地测试实例没有密钥卷）。
// 覆盖：① 卷表出现「详情」按钮并打开抽屉；② 规则表按后端 settings[] 渲染；
// ③ 新增规则 → 本地校验 → 保存时 PUT 到正确端点且载荷形状正确；④ 缺 max_size 时本地拦截不发请求。
package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestVolumeSettings_DetailDrawerEditsRules 钉住卷详情抽屉的规则编辑闭环。
func TestVolumeSettings_DetailDrawerEditsRules(t *testing.T) {
	// sproxy:serial: Playwright 浏览器 fixture 共享页面/路由拦截状态，串行更稳。
	baseURL, _, cleanup := testServerCfg(t, nil)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	var putHits atomic.Int64
	var putBody atomic.Value

	volumesBody := `{"volumes":[{"name":"vault","type":"secretdata","mode":"","capacity":0,"usage":0,"state":"healthy"}]}`
	detailBody := `{"volume":{"name":"vault","type":"secretdata","mode":"","capacity":0,"usage":0,"state":"healthy"},
	  "capabilities":["settings"],
	  "settings":[{"key":"block_policy","label":"分块规则","read_only":false,
	    "fields":[{"key":"rules","label":"规则","type":"rules","required":false}],
	    "value":{"version":3,"rules":[{"path_regex":"^archive/.*\\.zip$","max_size":20971520,"mode":"single","priority":100}]}}]}`

	jsonRoute := func(pattern, body string) {
		if err := page.Route(pattern, func(route playwright.Route) {
			_ = route.Fulfill(playwright.RouteFulfillOptions{
				Status:  playwright.Int(http.StatusOK),
				Body:    playwright.String(body),
				Headers: map[string]string{"Content-Type": "application/json"},
			})
		}); err != nil {
			t.Fatalf("Route %s: %v", pattern, err)
		}
	}
	jsonRoute("**/api/volumes", volumesBody)
	jsonRoute("**/api/volumes/vault/detail**", detailBody)
	if err := page.Route("**/api/volumes/vault/settings/*", func(route playwright.Route) {
		putHits.Add(1)
		if p, perr := route.Request().PostData(); perr == nil && p != "" {
			putBody.Store(p)
		}
		_ = route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(http.StatusOK),
			Body:    playwright.String(`{"success":true,"version":4}`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route settings: %v", err)
	}

	if _, err := page.Goto(baseURL+"/ui/v2/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("导航: %v", err)
	}
	// 先进控制台（分区按钮在控制台视图中，默认隐藏），再切到「卷」分区。
	if err := page.Locator(".nav[data-route=console]").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("进入控制台: %v", err)
	}
	if err := waitLoc(page, `button[data-cs="volumes"]`, playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("等待卷分区按钮: %v", err)
	}
	if err := page.Locator(`button[data-cs="volumes"]`).First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("点击卷分区: %v", err)
	}

	// ① 卷行「详情」按钮 → 抽屉可见
	detailBtn := page.Locator(`#v2-vl-body button[data-volume="vault"]`)
	if err := detailBtn.WaitFor(playwright.LocatorWaitForOptions{
		State: playwright.WaitForSelectorStateVisible, Timeout: playwright.Float(8000),
	}); err != nil {
		t.Fatalf("等待详情按钮: %v", err)
	}
	if err := detailBtn.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("点击详情: %v", err)
	}
	if err := waitLoc(page, "#v2-vdrawer.show", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("抽屉未打开: %v", err)
	}

	// ② 规则表按后端 settings 渲染（1 条，路径正则回显）
	if err := waitLoc(page, "#v2-vd-sections tbody tr", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("规则表未渲染: %v", err)
	}
	firstRegex, err := page.Locator("#v2-vd-sections tbody tr").First().Locator("input").First().InputValue()
	if err != nil {
		t.Fatalf("读规则正则: %v", err)
	}
	if firstRegex != `^archive/.*\.zip$` {
		t.Fatalf("规则正则回显不符: %q", firstRegex)
	}

	// ④ 新增一条 single 但填非法 max_size → 本地校验拦截，不发 PUT
	if err := page.Locator("#v2-vd-add").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("点击新增规则: %v", err)
	}
	rows := page.Locator("#v2-vd-sections tbody tr")
	if n, cerr := rows.Count(); cerr != nil || n != 2 {
		t.Fatalf("新增后应有 2 行，得到 %v (err=%v)", n, cerr)
	}
	// 新增行追加在末尾（fillRuleRows 保持行序，不在编辑时就地重排）。给它填正则可编译但缺 max_size。
	newRow := rows.Last().Locator("input")
	if err := newRow.Nth(0).Fill(`^photos/`); err != nil {
		t.Fatalf("填正则: %v", err)
	}
	if err := page.Locator("#v2-vd-save").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("点击保存: %v", err)
	}
	if err := waitLoc(page, "#v2-vd-rule-errors:not([hidden])", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("缺 max_size 应显示本地校验错误: %v", err)
	}
	if putHits.Load() != 0 {
		t.Fatalf("本地校验失败不应发 PUT，got %d", putHits.Load())
	}

	// ③ 补齐 max_size → 保存 → PUT 发出且载荷含 max_size
	if err := newRow.Nth(2).Fill("20MiB"); err != nil {
		t.Fatalf("填 max_size: %v", err)
	}
	if err := page.Locator("#v2-vd-save").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("再次保存: %v", err)
	}
	if err := waitLoc(page, "#v2-vd-rule-errors", playwright.WaitForSelectorStateHidden, 5000); err != nil {
		t.Fatalf("补齐后错误应隐藏: %v", err)
	}
	if putHits.Load() != 1 {
		t.Fatalf("应发 1 次 PUT，got %d", putHits.Load())
	}
	raw, _ := putBody.Load().(string)
	if !strings.Contains(raw, `"max_size"`) || !strings.Contains(raw, `"mode":"single"`) {
		t.Fatalf("PUT 载荷形状不符: %s", raw)
	}
	var payload struct {
		Rules []map[string]any `json:"rules"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("PUT 载荷非法 JSON: %v (%s)", err, raw)
	}
	if len(payload.Rules) != 2 {
		t.Fatalf("应提交 2 条规则，得到 %d", len(payload.Rules))
	}
}
