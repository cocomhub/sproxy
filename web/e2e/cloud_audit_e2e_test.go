// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// cloud_audit_e2e_test.go — 云端下载 + 审计面板 Web UI 真交互 E2E（PR-F2 任务 4）。
//
// 云下载走真实两段式：`#cloud-submit-btn` = 进入预览（替换输入行为文件名输入 +
// `#cloud-preview-confirm-btn`），确认后才发 POST /api/cloud/download（body 含
// url/filename）——杜绝「停在 preview 就算通过」的 false-green。完成态经 3s 轮询渲染，
// 断言留 ≥10s。审计用 Go 侧真实 PUT /api/config 事件 seed，断网络响应 + 表格行文本。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/mxschmitt/playwright-go"
)

// TestCloudDownload_SubmitCompleteRemove 提交 → 预览确认 → 完成（轮询）→ 删除。
func TestCloudDownload_SubmitCompleteRemove(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		// 回环源需放行（默认 false 会 403）。
		c.CloudDownloadAllowPrivate = true
	})
	defer cleanup()

	srcContent := bytes.Repeat([]byte("c"), 1024)
	srcURL, stopSrc := startFileSource(t, "cloud-src.bin", srcContent)
	defer stopSrc()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	openTransferPage(t, page)
	// 「仅提交」只进入预览（不发请求）；预览证据：确认按钮出现 + 自动生成文件名。
	previewCloudURL(t, page, srcURL, "cloud-src.bin")
	// 确认提交：POST /api/cloud/download，body 的 url 必须等于源 URL（证明前端把输入交给 API）。
	req := confirmCloudDownload(t, page)
	if got := req.Method(); got != "POST" {
		t.Errorf("cloud download method = %q, want POST", got)
	}
	var cloudReq struct {
		URL      string `json:"url"`
		Filename string `json:"filename"`
	}
	requestJSON(t, req, &cloudReq)
	if cloudReq.URL != srcURL || cloudReq.Filename != "cloud-src.bin" {
		t.Fatalf("cloud download body = %+v, want {url:%q filename:cloud-src.bin}", cloudReq, srcURL)
	}

	// 完成（3s 轮询）：先等「已完成」汇总出现（≥10s 余量），再展开分组断言文件名与任务态。
	if werr := waitLoc(page, "#transfer-body", playwright.WaitForSelectorStateVisible, 10000); werr != nil {
		t.Fatalf("#transfer-body 未显示: %v", werr)
	}
	waitTextVisible(t, page, "#transfer-body", "已完成", 15000)

	// 停掉 3s 轮询再操作：renderTransferChannel() 每次重建 #transfer-body，会把已完成
	// 分组的 <details> 重置为折叠——展开与点删除之间若撞上重建则按钮不可见/不可点。
	// 停轮询后 DOM 不再被重建（删除处理器自身会 refreshCloudTasks()，UI 仍更新）。
	if _, sErr := page.Evaluate("stopCloudPolling()"); sErr != nil {
		t.Fatalf("stopCloudPolling: %v", sErr)
	}
	// 已完成项被折叠在 <details> 内；展开后行文本才可读、操作按钮才可点。
	if cerr := page.Locator("#transfer-body summary").First().Click(); cerr != nil {
		t.Fatalf("展开已完成分组: %v", cerr)
	}
	waitTextVisible(t, page, "#transfer-body", "cloud-src.bin", 10000)

	// Go 侧佐证任务终态（网络证据）。
	assertCloudTaskCompleted(t, baseURL, "cloud-src.bin")

	// 删除：DELETE /api/cloud/tasks/{id} → 行消失。
	req2, err := page.ExpectRequest("**/api/cloud/tasks/*", func() error {
		return clickFirstCloudRemove(page)
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到 DELETE /api/cloud/tasks/{id}（删除未接线？）: %v", err)
	}
	if got := req2.Method(); got != "DELETE" {
		t.Errorf("cloud delete method = %q, want DELETE", got)
	}
	waitTextGone(t, page, "#transfer-body", "cloud-src.bin", 10000)
}

// openTransferPage 打开云端下载传输页（#cloud-btn → #transfer-page 可见）。
func openTransferPage(t *testing.T, page playwright.Page) {
	t.Helper()
	if err := page.Locator("#cloud-btn").Click(); err != nil {
		t.Fatalf("click cloud-btn: %v", err)
	}
	if err := waitLoc(page, "#transfer-page", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("transfer-page 未显示: %v", err)
	}
}

// previewCloudURL 填 URL → 点「仅提交」进入预览（不发请求），断言确认按钮与自动生成文件名。
func previewCloudURL(t *testing.T, page playwright.Page, srcURL, wantName string) {
	t.Helper()
	if err := page.Locator("#cloud-url").Fill(srcURL); err != nil {
		t.Fatalf("fill cloud-url: %v", err)
	}
	if err := page.Locator("#cloud-submit-btn").Click(); err != nil {
		t.Fatalf("click cloud-submit-btn: %v", err)
	}
	if err := waitLoc(page, "#cloud-preview-confirm-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("预览确认按钮未出现（preview 未接线？）: %v", err)
	}
	previewName, err := page.Locator(".cloud-preview-filename").First().InputValue()
	if err != nil {
		t.Fatalf("读取预览文件名: %v", err)
	}
	if previewName != wantName {
		t.Fatalf("预览文件名 = %q, want %s（safeDefaultFromURL 未接线）", previewName, wantName)
	}
}

// confirmCloudDownload 点预览确认按钮 → 捕获 POST /api/cloud/download 请求并返回。
func confirmCloudDownload(t *testing.T, page playwright.Page) playwright.Request {
	t.Helper()
	req, err := page.ExpectRequest("**/api/cloud/download", func() error {
		return page.Locator("#cloud-preview-confirm-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/cloud/download（预览确认未接线？）: %v", err)
	}
	return req
}

// clickFirstCloudRemove 点击首个 .cloud-remove-btn；不可见则先展开 <details>（幂等兜底，
// 防任何残余重建导致按钮不可点）。
func clickFirstCloudRemove(page playwright.Page) error {
	btn := page.Locator("#transfer-body .cloud-remove-btn").First()
	if vis, verr := btn.IsVisible(); verr != nil || !vis {
		if cerr := page.Locator("#transfer-body summary").First().Click(); cerr != nil {
			return cerr
		}
	}
	return btn.Click()
}

// assertCloudTaskCompleted 佐证云端任务已 completed（Go 侧网络证据）。
func assertCloudTaskCompleted(t *testing.T, baseURL, wantFile string) {
	t.Helper()
	tresp, err := http.Get(baseURL + "/api/cloud/tasks")
	if err != nil {
		t.Fatalf("GET /api/cloud/tasks: %v", err)
	}
	defer tresp.Body.Close()
	var tasksPayload struct {
		Tasks []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
		} `json:"tasks"`
	}
	if jerr := json.NewDecoder(tresp.Body).Decode(&tasksPayload); jerr != nil {
		t.Fatalf("解析 /api/cloud/tasks: %v", jerr)
	}
	for _, it := range tasksPayload.Tasks {
		if it.Filename == wantFile && it.Status == "completed" {
			return
		}
	}
	t.Fatalf("未找到 completed 的 %s 任务: %+v", wantFile, tasksPayload.Tasks)
}

// TestCloudDownload_Cancel 挂起源 → 任务停在下载中 → 取消（POST /cancel）→「已取消」。
func TestCloudDownload_Cancel(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
	})
	defer cleanup()

	srcURL, stopSrc := startStallingSource(t, "stall.bin")
	defer stopSrc()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#cloud-btn").Click(); err != nil {
		t.Fatalf("click cloud-btn: %v", err)
	}
	if err := waitLoc(page, "#transfer-page", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("transfer-page 未显示: %v", err)
	}
	if err := page.Locator("#cloud-url").Fill(srcURL); err != nil {
		t.Fatalf("fill cloud-url: %v", err)
	}
	if err := page.Locator("#cloud-submit-btn").Click(); err != nil {
		t.Fatalf("click cloud-submit-btn: %v", err)
	}
	if _, err := page.ExpectRequest("**/api/cloud/download", func() error {
		return page.Locator("#cloud-preview-confirm-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("未观察到 POST /api/cloud/download: %v", err)
	}

	// 行出现（等待中/下载中）——挂起源保证任务不会立即完成。
	waitTextVisible(t, page, "#transfer-body", "stall.bin", 12000)

	// 取消：POST /api/cloud/tasks/{id}/cancel。
	req, err := page.ExpectRequest("**/api/cloud/tasks/*/cancel", func() error {
		return page.Locator("#transfer-body .cloud-cancel-btn").First().Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/cloud/tasks/{id}/cancel（取消未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("cancel method = %q, want POST", got)
	}
	if !strings.HasSuffix(req.URL(), "/cancel") {
		t.Errorf("cancel URL = %q, want 以 /cancel 结尾", req.URL())
	}

	waitTextVisible(t, page, "#transfer-body", "已取消", 12000)
}

// TestCloudDownload_DamagedBadge_RealLink 完整性徽章**真实链路**：损坏 png 源 →
// 服务端 ImageChecker 语义校验失败 → completed+damaged → 真实 GET /api/cloud/tasks
// 返回 integrity_status=damaged → DOM 渲染「完整性异常」徽章。区别于 ui_e2e_test.go
// 的注入伪造对象变体（绕过服务端），本用例端到端验证「服务端判定 → API → DOM」接线。
func TestCloudDownload_DamagedBadge_RealLink(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
	})
	defer cleanup()

	// 损坏 png：名为 .png 但内容非图片 → ImageChecker 语义解码失败 → damaged。
	srcContent := []byte("corrupt-integrity-e2e-payload-not-a-real-png")
	srcURL, stopSrc := startFileSource(t, "damaged.png", srcContent)
	defer stopSrc()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	openTransferPage(t, page)
	previewCloudURL(t, page, srcURL, "damaged.png")
	confirmCloudDownload(t, page)

	// 轮询等完成 + 真实 API 的 integrity_status=damaged（服务端语义判定）。
	testutil.WaitFor(t, 30*time.Second, func() bool {
		tresp, err := http.Get(baseURL + "/api/cloud/tasks")
		if err != nil {
			return false
		}
		defer tresp.Body.Close()
		var tasksPayload struct {
			Tasks []struct {
				Filename        string `json:"filename"`
				Status          string `json:"status"`
				IntegrityStatus string `json:"integrity_status"`
			} `json:"tasks"`
		}
		if jerr := json.NewDecoder(tresp.Body).Decode(&tasksPayload); jerr != nil {
			return false
		}
		for _, it := range tasksPayload.Tasks {
			if it.Filename == "damaged.png" && it.Status == "completed" && it.IntegrityStatus == "damaged" {
				return true
			}
		}
		return false
	}, func() string { return "服务端应判 completed+damaged" })

	// DOM 徽章（真实 API → normalizeCloudTaskItem → buildIntegrityBadge → 「完整性异常」）。
	// completed 任务被折叠在分组 <details> 内（summary 文本可读、行文本不可读），先展开
	// 分组再断言徽章（同 ui_e2e_test.go 注入变体的展开模式）。
	waitTextVisible(t, page, "#transfer-body", "已完成", 15000)
	if cerr := page.Locator("#transfer-body summary").First().Click(); cerr != nil {
		t.Fatalf("展开已完成分组: %v", cerr)
	}
	waitTextVisible(t, page, "#transfer-body", "damaged.png", 15000)
	waitTextVisible(t, page, "#transfer-body", "完整性异常", 15000)
}

// TestAudit_RendersSeededEvent 审计面板：Go 侧真实 config_update 事件 seed → 点审计 tab
// 触发 GET /api/audit?limit=200 → 断响应事件 + 表格行 + tab 切换真实生效。
func TestAudit_RendersSeededEvent(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	// 先于 goto seed：PUT /api/config 无条件记一条 config_update success。
	if st := seedConfigUpdate(t, baseURL); st != http.StatusOK {
		t.Fatalf("seed config_update status=%d, want 200", st)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	// 点击审计 tab → GET /api/audit?limit=200（断响应含 seed 事件 + 表格行 + tab 切换真实生效）。
	assertAuditSeededEvent(t, page, "config_update", "success")
	assertAuditPanelDOM(t, page)
}

// openStatsModal 打开监控弹窗（点 #stats-btn 并等 #stats-modal 可见）。
func openStatsModal(t *testing.T, page playwright.Page) {
	t.Helper()
	if err := page.Locator("#stats-btn").Click(); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("stats-modal 未显示: %v", err)
	}
}

// assertAuditSeededEvent 点击审计 tab → 捕获 GET /api/audit?limit=200 响应，断言事件列表
// 含 action/result 匹配的 seed 事件。
func assertAuditSeededEvent(t *testing.T, page playwright.Page, action, result string) {
	t.Helper()
	resp, err := page.ExpectResponse("**/api/audit?limit=200", func() error {
		return page.Locator("#audit-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击审计 tab 未触发 GET /api/audit: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("GET /api/audit status = %d, want 200", got)
	}
	var auditPayload struct {
		Events []struct {
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"events"`
	}
	if jerr := resp.JSON(&auditPayload); jerr != nil {
		t.Fatalf("解析 /api/audit 响应: %v", jerr)
	}
	found := false
	for _, ev := range auditPayload.Events {
		if ev.Action == action && ev.Result == result {
			found = true
		}
	}
	if !found {
		t.Fatalf("审计响应未含 %s %s 事件: %+v", action, result, auditPayload.Events)
	}
}

// assertAuditPanelDOM 断言审计表格行出现（非空态），tab 切换真实生效（audit 可见 / stats 隐藏）。
func assertAuditPanelDOM(t *testing.T, page playwright.Page) {
	t.Helper()
	if werr := waitLoc(page, "#audit-panel table tbody tr", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		body, _ := page.Locator("#audit-panel").InnerText()
		t.Fatalf("审计表格未渲染: %v；panel=%q", werr, body)
	}
	if vis, verr := page.Locator("#audit-panel").IsVisible(); verr != nil || !vis {
		t.Fatalf("audit-panel 应可见 (err=%v)", verr)
	}
	if vis, _ := page.Locator("#stats-panel").IsVisible(); vis {
		t.Error("切到审计 tab 后 #stats-panel 应隐藏")
	}
	panelTxt, _ := page.Locator("#audit-panel").InnerText()
	for _, want := range []string{"config_update", "操作", "主体"} {
		if !strings.Contains(panelTxt, want) {
			t.Errorf("审计面板缺 %q:\n%s", want, panelTxt)
		}
	}
}
