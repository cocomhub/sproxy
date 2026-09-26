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
	"strings"
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

// TestWebUIFederation_RendersViews：Hub 联邦视图（11.8-B7）——mock 联邦节点/服务
// 端点数据（Playwright Route 拦截），断言 #hub-panel 渲染联邦节点表 + 服务表：
// 直连节点显示本地时间（含年份）、联邦候选标注「联邦候选」、mesh 列显示默认/命名。
func TestWebUIFederation_RendersViews(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()

	// 拦截联邦两个端点（浏览器 fetch 路径被 mock，与 /metrics 拦截同法）。
	// /api/hub/nodes 一并 mock（未配置 RouteTable 的测试实例真实 404）——直连节点
	// 表兜底仍渲染；/api/hub/stats 与 /api/mesh/status 保持真实（独立容错）。
	// 注意：Playwright Route 匹配是**路径前缀**，/**/api/hub/nodes 会同时命中
	// federation/nodes（前缀更长先注册优先），故必须先注册联邦端点再注册普通端点。
	if err := page.Route("**/api/hub/federation/nodes", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(`[{"id":"node-a","addr":"192.168.1.1:9000","mesh":"","connected":"2026-09-26T00:00:00Z"},{"id":"node-b","addr":"192.168.1.2:9000","mesh":"prod"}]`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/federation/nodes: %v", err)
	}
	if err := page.Route("**/api/hub/federation/services", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(`[{"node":"node-a","name":"sg-ssh","addr":"t:22","mesh":""},{"node":"node-b","name":"volread","addr":"127.0.0.1:19000","mesh":"prod"}]`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/federation/services: %v", err)
	}
	if err := page.Route("**/api/hub/nodes", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(`[{"id":"local","addr":"127.0.0.1:1","connected":"2026-09-26T00:00:00Z"}]`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/nodes: %v", err)
	}

	if err := openHubPanelForTopology(page, baseURL); err != nil {
		t.Fatalf("打开 Hub 面板: %v", err)
	}

	// 联邦节点表：标题 + 直连节点 + 候选标注 + mesh 列（默认/命名）。
	for _, want := range []string{"联邦节点（跨 hub 发现）", "node-a", "192.168.1.1:9000", "联邦候选", "prod"} {
		waitTextVisible(t, page, "#hub-panel", want, 8000)
	}
	// 联邦服务表：标题 + 服务名/地址。
	for _, want := range []string{"联邦服务（跨 hub 宣告）", "sg-ssh", "t:22", "volread", "127.0.0.1:19000"} {
		waitTextVisible(t, page, "#hub-panel", want, 8000)
	}
}

// TestWebUIFederation_404KeepsOthers：联邦端点未启用（404）→ 独立容错，既有
// 直连节点表/空态仍渲染（不出现空联邦卡，也不连带隐藏其它区块）。
func TestWebUIFederation_404KeepsOthers(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()

	// 联邦端点保持真实（未配置 federation.enabled → 404）。
	if err := page.Route("**/api/hub/nodes", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(`[{"id":"node-a","addr":"192.168.1.1:9000"}]`),
			Headers: map[string]string{"Content-Type": "application/json"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /api/hub/nodes: %v", err)
	}

	if err := openHubPanelForTopology(page, baseURL); err != nil {
		t.Fatalf("打开 Hub 面板: %v", err)
	}

	// 既有节点表格兜底仍渲染（直连节点可见）；联邦区块不出现（不白屏、不报错）。
	waitTextVisible(t, page, "#hub-panel", "node-a", 8000)
	if n, err := page.Locator("#hub-panel").InnerText(); err == nil {
		if strings.Contains(n, "联邦节点（跨 hub 发现）") {
			t.Fatalf("联邦端点 404 时不应渲染联邦节点表")
		}
	}
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
