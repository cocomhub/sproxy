// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// webui_topology_e2e_test.go — WebUI 节点拓扑真交互 E2E（roadmap 11.1-⑦）。
//
// Hub tab 点击 → 触发 GET /api/hub/nodes（服务端真实装配 RouteTable，带 RTT/quality）
// → #hub-panel 渲染 SVG 拓扑（hub 中心 + 3 叶子节点 + 边色/虚线）+ 既有节点表格兜底。
// 数据经 Playwright Route 拦截替换为固定样本：3 节点（快 RTT 绿边 / 慢 RTT 黄边 /
// 未知 RTT 虚线），断言边数、节点 id、图例、N/A 标注与表格「移除」按钮并存。

import (
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// openHubPanelForTopology 打开监控弹窗并切到 Hub tab（复用 mesh_status_e2e 的交互链）。
func openHubPanelForTopology(page playwright.Page, baseURL string) error {
	if _, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); gerr != nil {
		return gerr
	}
	if err := page.Locator("#stats-btn").Click(); err != nil {
		return err
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return err
	}
	if err := page.Locator("#hub-tab").Click(); err != nil {
		return err
	}
	return waitLoc(page, "#hub-panel", playwright.WaitForSelectorStateVisible, 8000)
}

// TestWebUITopology_RendersGraph：Hub 面板渲染 SVG 拓扑——3 节点（边色 + 虚线 N/A）+ 表格兜底。
func TestWebUITopology_RendersGraph(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()

	// 拦截 /api/hub/nodes：返回固定拓扑样本（浏览器 fetch 路径被 mock，
	// 与 /metrics 拦截同法；/api/hub/stats 与 /api/mesh/status 保持真实 200/空态）。
	mockNodes := `[{"id":"node-a","addr":"192.168.1.1:9000","quality":"healthy","rtt_ms":50},
	  {"id":"node-b","addr":"192.168.1.2:9000","quality":"degraded","rtt_ms":250},
	  {"id":"node-c","addr":"192.168.1.3:9000","quality":"stale","rtt_ms":-1}]`
	if err := page.Route("**/api/hub/nodes", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(mockNodes),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/nodes: %v", err)
	}

	if err := openHubPanelForTopology(page, baseURL); err != nil {
		t.Fatalf("打开 Hub 面板: %v", err)
	}
	if err := waitLoc(page, "#hub-panel svg", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("Hub 拓扑 svg 未渲染: %v", err)
	}

	// 边数 = 3（hub 中心到每个叶子的 line），叶子节点文本都在。
	lineCount, err := page.Locator("#hub-panel svg line").Count()
	if err != nil {
		t.Fatalf("统计拓扑边数: %v", err)
	}
	if lineCount != 3 {
		t.Fatalf("拓扑边数 = %d, want 3", lineCount)
	}
	for _, want := range []string{"node-a", "node-b", "node-c", "未知 RTT", "<100ms", "≥500ms"} {
		waitTextVisible(t, page, "#hub-panel", want, 8000)
	}

	// 表格兜底仍在（节点表格「移除」按钮），证明拓扑与既有表格并存不互斥。
	if err := waitLoc(page, "#hub-panel .hub-remove-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("Hub 节点表格兜底缺失: %v", err)
	}
}

// TestWebUITopology_EmptyNodesKeepsTable：无节点 → 不渲染空 svg，表格空态兜底不白屏。
func TestWebUITopology_EmptyNodesKeepsTable(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()

	if err := page.Route("**/api/hub/nodes", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(`[]`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/nodes: %v", err)
	}

	if err := openHubPanelForTopology(page, baseURL); err != nil {
		t.Fatalf("打开 Hub 面板: %v", err)
	}
	// 无节点：不渲染 svg，表格空态提示出现。
	svgCount, err := page.Locator("#hub-panel svg").Count()
	if err != nil {
		t.Fatalf("统计 svg: %v", err)
	}
	if svgCount != 0 {
		t.Fatalf("空节点不应渲染 svg, got %d", svgCount)
	}
	waitTextVisible(t, page, "#hub-panel", "暂无已连接节点", 8000)
}
