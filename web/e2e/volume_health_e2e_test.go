// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volume_health_e2e_test.go — WebUI 卷健康仪表真交互 E2E（Playwright + Chromium）。
//
// 验证两场景：
//   1. 有卷健康数据（h.metrics.RecordVolumeIO 注入样本）→ 卷面板渲染健康行
//      （卷名/操作/总次数/失败率/状态徽标）；degraded 卷出现告警样式。
//   2. 无样本数据 → 面板显示空态提示，不报错、不破坏卷面板其余部分。
//
// 卷健康数据源：#432 sproxy_volume_io_*（/metrics，公开无凭据）；面板在
// showVolumes() 时经 loadVolumeHealth 拉取渲染（30s 定时刷新）。

import (
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// openVolumesPanelForHealth 打开卷面板并等待卷健康区挂载（复用 openVolumesPanel 流程）。
func openVolumesPanelForHealth(page playwright.Page, baseURL string) error {
	if _, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); gerr != nil {
		return gerr
	}
	if err := page.Locator("#stats-btn").Click(); err != nil {
		return err
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return err
	}
	if err := page.Locator("#volumes-tab").Click(); err != nil {
		return err
	}
	if err := waitLoc(page, "#volumes-panel", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		return err
	}
	// 卷健康区容器（volumeHealthSectionHtml 挂载）渲染。
	return waitLoc(page, "#volume-health-list", playwright.WaitForSelectorStateVisible, 8000)
}

// TestVolumeHealthE2E_RenderWithData 有样本：Playwright Route 拦截 /metrics 返回
// main/upload（healthy）+ disk2/download（degraded，失败率 20%）两卷样本 →
// 卷面板出现健康行与 degraded 告警行（真浏览器验证渲染路径，不依赖服务端样本）。
func TestVolumeHealthE2E_RenderWithData(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()
	// 拦截 /metrics：返回带 volume_io 样本的 Prometheus 文本（浏览器 fetch 拉取路径被 mock）。
	mockMetrics := "# TYPE sproxy_volume_io_total counter\n" +
		"sproxy_volume_io_total{volume=\"main\",op=\"upload\"} 3\n" +
		"sproxy_volume_io_total{volume=\"disk2\",op=\"download\"} 5\n" +
		"# TYPE sproxy_volume_io_failures_total counter\n" +
		"sproxy_volume_io_failures_total{volume=\"disk2\",op=\"download\"} 1\n"
	if err := page.Route("**/metrics", func(route playwright.Route) {
		route.Fulfill(playwright.RouteFulfillOptions{
			Status:  playwright.Int(200),
			Body:    playwright.String(mockMetrics),
			Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		})
	}); err != nil {
		t.Fatalf("Route 拦截 /metrics: %v", err)
	}

	if err := openVolumesPanelForHealth(page, baseURL); err != nil {
		t.Fatalf("打开卷面板: %v", err)
	}
	// 卷健康面板渲染出数据行（等 loadVolumeHealth 的 fetch 完成）。
	if err := waitLoc(page, "#volume-health-list tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("卷健康行未出现: %v", err)
	}
	html, err := page.Locator("#volume-health-list").InnerText()
	if err != nil {
		t.Fatalf("读卷健康面板文本: %v", err)
	}
	if !strings.Contains(html, "main") || !strings.Contains(html, "upload") {
		t.Errorf("面板缺 main/upload 行: %q", html)
	}
	if !strings.Contains(html, "healthy") {
		t.Errorf("main/upload 应 healthy 徽标: %q", html)
	}
	if !strings.Contains(html, "disk2") || !strings.Contains(html, "degraded") {
		t.Errorf("面板缺 disk2/degraded 行: %q", html)
	}
	// 失败率 1/5 = 20% 显示。
	if !strings.Contains(html, "20.00%") {
		t.Errorf("disk2/download 失败率应为 20.00%%: %q", html)
	}
}

// TestVolumeHealthE2E_EmptyState 无样本：面板显示空态提示，卷面板其它区不受影响。
func TestVolumeHealthE2E_EmptyState(t *testing.T) {
	t.Parallel()
	baseURL, _, _, cleanup := testServerCfgWithHandlers(t, nil)
	t.Cleanup(cleanup)

	page, closePage := pageFixture(t)
	defer closePage()
	if err := openVolumesPanelForHealth(page, baseURL); err != nil {
		t.Fatalf("打开卷面板: %v", err)
	}
	// 无样本 → 空态提示（fetch /metrics 返回 200 但无 volume_io 行）。
	if err := waitLoc(page, "#volume-health-list .empty-msg", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("卷健康空态提示未出现: %v", err)
	}
	txt, err := page.Locator("#volume-health-list").InnerText()
	if err != nil {
		t.Fatalf("读卷健康空态文本: %v", err)
	}
	if !strings.Contains(txt, "暂无卷健康数据") {
		t.Errorf("空态提示应含「暂无卷健康数据」: %q", txt)
	}
	// 卷面板其余部分（存储卷标题）不受影响。
	if err := waitLoc(page, "#volumes-panel", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("卷面板整体不可见: %v", err)
	}
}
