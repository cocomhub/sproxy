// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// quota_e2e_test.go —— 配额面板的**真浏览器** e2e（项目硬要求：Web UI 改动必须有 Playwright 用例）。
//
// 钉住的不变式（不是「页面上有字」这类弱断言）：
//   ① 配额区随**存储卷面板**一起渲染（骨架 → 数据），**不得**停在「加载中...」；
//   ② 渲染结果要么是配额表（`quota-table`）要么是**显式空态**（`empty-msg`）——
//      后者覆盖「旧服务端无 /api/quota」与「无权威数据」两种合法情形；
//   ③ 刷新按钮存在且可点击（交互可达）。

import (
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestE2E_QuotaPanel_RendersFederatedView 配额面板加载完成后必须呈现配额表或显式空态。
func TestE2E_QuotaPanel_RendersFederatedView(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 复用既有导航：/ui/ → #stats-btn → #volumes-tab → #volumes-panel（配额区在同一面板内）。
	if _, oerr := openVolumesPanel(page, baseURL); oerr != nil {
		t.Fatalf("打开存储卷面板: %v", oerr)
	}
	if lerr := waitLoc(page, "#quota-panel", playwright.WaitForSelectorStateVisible, 10000); lerr != nil {
		t.Fatalf("配额面板容器 #quota-panel 未出现: %v", lerr)
	}

	// 等待「加载中」消失（= 渲染完成）。**不用 time.Sleep**（R14 棘轮只减不增）——
	// 复用既有轮询助手 testutil.WaitForBool；断言「呈现配额表或显式空态」。
	panel := page.Locator("#quota-panel")
	rendered := func() bool {
		html, herr := panel.InnerHTML()
		if herr != nil || strings.Contains(html, "加载中") {
			return false
		}
		return strings.Contains(html, "quota-table") || strings.Contains(html, "empty-msg")
	}
	if !testutil.WaitForBool(10*time.Second, rendered) {
		html, _ := panel.InnerHTML()
		t.Fatalf("配额面板未渲染完成（既无配额表也无显式空态）: %s", html)
	}

	// 交互可达：刷新按钮必须存在且可点击。
	if lerr := waitLoc(page, "#quota-refresh", playwright.WaitForSelectorStateVisible, 5000); lerr != nil {
		t.Fatalf("配额刷新按钮 #quota-refresh 未出现: %v", lerr)
	}
	if cerr := page.Locator("#quota-refresh").Click(); cerr != nil {
		t.Fatalf("点击配额刷新按钮失败: %v", cerr)
	}
	if _, herr := panel.InnerHTML(); herr != nil {
		t.Fatalf("刷新后读取面板失败: %v", herr)
	}
}
