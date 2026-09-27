// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// credentials_sync_mesh_e2e_test.go Playwright e2e：
//   B2 凭据管理 tab（/api/credentials 面板渲染）
//   B3 同步冲突 tab（/api/sync/conflicts 面板渲染）
//   B8 Mesh 状态 tab（/api/mesh/status 面板渲染）
// 真浏览器 + 真实服务端（testServer），验证 3 个新 tab 可切换 + 面板内容渲染。

import (
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestStatsPanel_CredentialsTab 凭据 tab：切换 → 面板渲染（admin-only；测试服务无
// admin 凭据 → 403 错误占位；无凭据 Ring 装配时也可能空提示）。断言 tab 可切换即可。
func TestStatsPanel_CredentialsTab(t *testing.T) {
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
	if err := page.Locator("#credentials-tab").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click credentials-tab: %v", err)
	}
	// 面板出现（加载完成/错误占位都算——非「加载中」）。
	if err := waitLoc(page, "#credentials-panel", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("credentials-panel 未显示: %v", err)
	}
	vis, _ := page.Locator("#credentials-panel").IsVisible()
	if !vis {
		t.Fatal("凭据 tab 切换后面板应可见")
	}
}

// TestStatsPanel_SyncConflictsTab 同步冲突 tab：切换 → GET /api/sync/conflicts。
func TestStatsPanel_SyncConflictsTab(t *testing.T) {
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
	resp, err := page.ExpectResponse("**/api/sync/conflicts", func() error {
		return page.Locator("#sync-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击同步冲突 tab 未触发 /api/sync/conflicts: %v", err)
	}
	if got := resp.Status(); got != 200 && got != 400 {
		t.Fatalf("GET /api/sync/conflicts status = %d, want 200（未配置同步时 400 可接受）", got)
	}
	// 面板渲染（空态、表格或未配置占位）。
	if err := waitLoc(page, "#sync-panel", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("sync-panel 未显示: %v", err)
	}
}

// TestStatsPanel_MeshStatusTab Mesh 状态 tab：切换 → GET /api/mesh/status。
func TestStatsPanel_MeshStatusTab(t *testing.T) {
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
	resp, err := page.ExpectResponse("**/api/mesh/status", func() error {
		return page.Locator("#mesh-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击 Mesh tab 未触发 /api/mesh/status: %v", err)
	}
	if got := resp.Status(); got != 200 {
		t.Fatalf("GET /api/mesh/status status = %d, want 200", got)
	}
	// 注：testServer 无 mesh 配置 → appRender.meshStatusHtml 返回空串（设计使然），
	// 面板为无内容 div（Playwright 判定尺寸 0 不可见）——改为断言 panel 存在 +
	// display 已切换 block（请求成功由 ExpectResponse 已证）。
	panel, _ := page.Locator("#mesh-panel").Count()
	if panel == 0 {
		t.Fatal("#mesh-panel 不存在")
	}
	disp, _ := page.Locator("#mesh-panel").Evaluate("el => el.style.display", nil)
	if disp != "block" {
		t.Fatalf("mesh-panel display = %v, want block", disp)
	}
}
