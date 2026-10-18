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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestE2E_QuotaPanel_RendersFederatedView 配额面板加载后必须渲染**数据行**（装配了真实
// 卷 ⇒ /api/quota 返回非空 volumes）：真实卷名出现在面板、渲染的是配额表而非空态/失败态。
//
// 此前 e2e 只覆盖空态路径（testServer 无 Volumes 装配 ⇒ /api/quota 恒空），逐卷字段契约
// bug（v.name→v.volume）漏网；且「quota-table 或 empty-msg」判据在 loadQuota **恒失败**
// 时也通过（catch 分支产 empty-msg，判据非空自证失效）。
func TestE2E_QuotaPanel_RendersFederatedView(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(cfg *server.Config) {
		cfg.Volumes = []server.VolumeConfig{
			{Name: "quota-vol", Root: filepath.Join(t.TempDir(), "qvol"), VolCapacity: server.ByteSize(1 << 30)},
		}
	})
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

	// 等待「加载中」消失，且**真实卷名出现**（数据行路径——不是空态）。
	panel := page.Locator("#quota-panel")
	rendered := func() bool {
		html, herr := panel.InnerHTML()
		if herr != nil || strings.Contains(html, "加载中") {
			return false
		}
		return strings.Contains(html, "quota-vol")
	}
	if !testutil.WaitForBool(10*time.Second, rendered) {
		html, _ := panel.InnerHTML()
		t.Fatalf("配额面板未渲染出数据行（卷名 quota-vol 未出现）: %s", html)
	}
	// 渲染的是配额表（非 empty-msg 失败态）：判据非空自证——loadQuota 恒失败也产
	// empty-msg，故必须断言 quota-table + 卷名同时成立。
	html, _ := panel.InnerHTML()
	if !strings.Contains(html, "quota-table") {
		t.Fatalf("渲染结果应为配额表（非 empty-msg 失败态）: %s", html)
	}

	// 交互可达：刷新按钮存在且可点击，点击后重新渲染仍含数据行。
	if lerr := waitLoc(page, "#quota-refresh", playwright.WaitForSelectorStateVisible, 5000); lerr != nil {
		t.Fatalf("配额刷新按钮 #quota-refresh 未出现: %v", lerr)
	}
	if cerr := page.Locator("#quota-refresh").Click(); cerr != nil {
		t.Fatalf("点击配额刷新按钮失败: %v", cerr)
	}
	if !testutil.WaitForBool(10*time.Second, func() bool {
		h2, herr := panel.InnerHTML()
		return herr == nil && strings.Contains(h2, "quota-vol")
	}) {
		html, _ := panel.InnerHTML()
		t.Fatalf("刷新后配额面板应重新渲染数据行: %s", html)
	}
}
