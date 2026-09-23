// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// notify_panel_test.go Playwright e2e：stats 面板通知区渲染。
// 打开监控弹窗 stats tab → 断言「最近通知」区块或空提示存在（notify 未装配时）。

import (
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// TestStatsPanel_NotifySection 监控弹窗含通知区块。
func TestStatsPanel_NotifySection(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := page.Click("#stats-btn"); err != nil {
		t.Fatalf("click stats: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	html, err := page.InnerHTML("#stats-panel")
	if err != nil {
		t.Fatalf("stats panel: %v", err)
	}
	// 通知区块（未装配 → 空提示；装配 → 最近通知）。
	if !strings.Contains(html, "最近通知") && !strings.Contains(html, "暂无通知记录") {
		t.Fatalf("stats-panel 应含通知区块: %s", clipStr(html, 300))
	}
}

// clipStr 截断长字符串（调试输出）。
func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
