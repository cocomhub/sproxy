// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// console_layout_e2e_test.go — 控制台（原监控弹窗）全屏化 + 分区导航 + 深链 E2E（2026-11-22）。
//
// 断言：① 面板尺寸接近视口（宽 ≥ 60vw、高 ≥ 70vh），不再是 500px 小窗；
// ② 左侧导航含全部分区；③ 点击「云端任务」切分区并写 hash；④ **刷新后仍停在云端任务**（深链）。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestConsoleLayout_FullscreenNavAndDeepLink 覆盖全屏尺寸/导航/深链刷新。
func TestConsoleLayout_FullscreenNavAndDeepLink(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// ① 尺寸：读取 shell 的 boundingBox，必须远大于旧 500×80vh。
	box, err := page.Locator("#console-shell").BoundingBox()
	if err != nil || box == nil {
		t.Fatalf("读取控制台尺寸: %v", err)
	}
	viewport := page.ViewportSize()
	if viewport == nil {
		t.Fatal("viewport 未知")
	}
	minW := float64(viewport.Width) * 0.6
	minH := float64(viewport.Height) * 0.7
	if box.Width < minW || box.Height < minH {
		t.Fatalf("控制台仍过小: %.0fx%.0f（视口 %dx%d）",
			box.Width, box.Height, viewport.Width, viewport.Height)
	}

	// ② 导航：侧栏含云端任务/待处理事项/审计 等分区。
	for _, label := range []string{"云端任务", "待处理事项", "审计", "配置"} {
		if err := waitLoc(page, "#stats-tab-bar button:has-text('"+label+"')",
			playwright.WaitForSelectorStateVisible, 5000); err != nil {
			t.Fatalf("侧栏缺少分区 %q: %v", label, err)
		}
	}

	// ③ 切到云端任务：面板可见 + hash 深链。
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("云端任务面板未显示: %v", err)
	}
	if h, _ := page.Evaluate("() => location.hash", nil); h != "#/console/cloud-tasks" {
		t.Fatalf("深链 hash = %v，want #/console/cloud-tasks", h)
	}

	// ④ 刷新后仍在云端任务（深链生效）。
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新: %v", err)
	}
	openStatsModal(t, page)
	if err := waitLoc(page, "#cloud-tasks-panel", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("刷新后未恢复云端任务分区: %v", err)
	}
}
