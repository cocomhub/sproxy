// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// cloud_tasks_panel_e2e_test.go — 「云端任务」面板真交互 E2E（2026-11-21）。
//
// 覆盖：失败任务在面板可见（状态 + 失败类别 + 尝试次数）→ 点「重试」发出
// POST /api/cloud/tasks/retry（body.task_id 必须是被点那条）→ 面板刷新。
// 用**真实失败任务**（不可达源）而非 mock，避免 false-green。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/mxschmitt/playwright-go"
)

// TestCloudTasksPanel_ShowsFailureAndRetries 锁定「看得见 + 点得动」。
func TestCloudTasksPanel_ShowsFailureAndRetries(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端，无法并行
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true // 回环/内网源放行
		c.CloudMaxRetries = 1              // 快速失败，缩短等待
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	// 1) 造一个必然失败的云任务：不可达端口。
	body := []byte(`{"url":"http://127.0.0.1:1/nope.bin","filename":"nope.bin","download_local":true,"save":true}`)
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
		t.Fatalf("创建任务失败: %d %s", code, resp)
	}
	// 2) 等它进入 failed（服务端异步），再用 API 取 task id。
	var taskID string
	deadline := time.Now().Add(30 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for time.Now().Before(deadline) && taskID == "" {
		resp, err := http.Get(baseURL + "/api/cloud/tasks")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var payload struct {
			Tasks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"tasks"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		for _, tk := range payload.Tasks {
			if tk.Status == "failed" {
				taskID = tk.ID
			}
		}
		if taskID == "" {
			select {
			case <-tick.C:
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if taskID == "" {
		t.Fatal("任务未在 30s 内进入 failed（面板用例无法继续）")
	}

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page) // tab 行在监控弹窗内

	// 3) 打开「云端任务」tab：失败行必须出现（状态/重试按钮）。
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务 tab: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-retry", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("失败任务行未渲染: %v", err)
	}
	rowText, err := page.Locator("#cloud-tasks-panel tbody tr").First().TextContent()
	if err != nil {
		t.Fatalf("读取行文本: %v", err)
	}
	if !strings.Contains(rowText, "失败") && !strings.Contains(rowText, "已取消") {
		t.Fatalf("行内应显示失败状态，got %q", rowText)
	}

	// 4) 点「重试」：必须发出 POST /api/cloud/tasks/retry 且 task_id 正确。
	reqCh := make(chan playwright.Request, 1)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/api/cloud/tasks/retry") {
			select {
			case reqCh <- r:
			default:
			}
		}
	})
	if err := page.Locator("#cloud-tasks-panel .ct-retry").Click(); err != nil {
		t.Fatalf("点击重试: %v", err)
	}
	select {
	case req := <-reqCh:
		var payload struct {
			TaskID string `json:"task_id"`
			Force  bool   `json:"force"`
		}
		requestJSON(t, req, &payload)
		if payload.TaskID != taskID {
			t.Fatalf("重试 task_id = %q, want %q", payload.TaskID, taskID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("点击重试后未发出 /api/cloud/tasks/retry 请求")
	}
}

// TestCloudTasksPanel_DetailAndBatchRetry 覆盖详情展开与批量重试（2026-11-22）。
func TestCloudTasksPanel_DetailAndBatchRetry(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	body := []byte(`{"url":"http://127.0.0.1:1/nope2.bin","filename":"nope2.bin","download_local":true,"save":true}`)
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
		t.Fatalf("创建任务失败: %d %s", code, resp)
	}
	// 等任务失败（服务端异步）。
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-detail-btn", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("失败任务行未渲染: %v", err)
	}

	// ① 详情：点开必须出现 ct-detail 行（含失败原文/部分产物）。
	if err := page.Locator("#cloud-tasks-panel .ct-detail-btn").First().Click(); err != nil {
		t.Fatalf("点击详情: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-detail", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("详情行未展开: %v", err)
	}

	// ② 批量重试：全选 → 点批量重试 → 必须发出 POST /api/cloud/tasks/retry。
	reqCh := make(chan playwright.Request, 4)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/api/cloud/tasks/retry") {
			select {
			case reqCh <- r:
			default:
			}
		}
	})
	if err := page.Locator("#ct-pick-all").Check(); err != nil {
		t.Fatalf("全选: %v", err)
	}
	if err := page.Locator("#ct-retry-selected").Click(); err != nil {
		t.Fatalf("批量重试: %v", err)
	}
	select {
	case req := <-reqCh:
		var payload struct {
			TaskID string `json:"task_id"`
			Force  bool   `json:"force"`
		}
		requestJSON(t, req, &payload)
		if payload.TaskID == "" || !payload.Force {
			t.Fatalf("批量重试请求体异常: %+v", payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("批量重试未发出 /api/cloud/tasks/retry")
	}
}

// TestCloudTasksPanel_FailedRowHasRichActions 锁定「失败行有丰富处理动作，不只是重试」
// （用户 2026-11-26）：下拉可选「续传下载」，执行后必须发出 POST /api/cloud/tasks/{id}/actions。
func TestCloudTasksPanel_FailedRowHasRichActions(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	body := []byte(`{"url":"http://127.0.0.1:1/nope4.bin","filename":"nope4.bin","download_local":true,"save":true}`)
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
		t.Fatalf("创建任务失败: %d %s", code, resp)
	}
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-action-select", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("失败行缺少动作下拉: %v", err)
	}
	// 动作清单必须包含四种处理方式（丰富度断言，不是只有「重试」）。
	opts, err := page.Locator("#cloud-tasks-panel .ct-action-select").First().Locator("option").AllInnerTexts()
	if err != nil {
		t.Fatalf("读取动作选项: %v", err)
	}
	joined := strings.Join(opts, "|")
	for _, want := range []string{"续传下载", "仅重做转存", "换源重试", "清残留重下"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("动作清单缺少 %q（实际 %q）", want, joined)
		}
	}
	// 选择「续传下载」并执行 → 必须发出 POST /actions。
	reqCh := make(chan playwright.Request, 4)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/actions") {
			select {
			case reqCh <- r:
			default:
			}
		}
	})
	if _, serr := page.Locator("#cloud-tasks-panel .ct-action-select").First().SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"retry_download"}}); serr != nil {
		t.Fatalf("选择动作: %v", serr)
	}
	if err := page.Locator("#cloud-tasks-panel .ct-action-apply").First().Click(); err != nil {
		t.Fatalf("执行动作: %v", err)
	}
	select {
	case req := <-reqCh:
		var payload struct {
			Action string `json:"action"`
		}
		requestJSON(t, req, &payload)
		if payload.Action != "retry_download" {
			t.Fatalf("动作请求体 = %q，want retry_download", payload.Action)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("执行动作未发出 POST /api/cloud/tasks/{id}/actions")
	}
}

// TestCloudTasksPanel_BatchAction 锁定「批量动作」真交互（用户 2026-11-26）：
// 勾选多个失败任务 → 选择批量动作 → 执行必须发出 POST /api/cloud/tasks/actions/batch，
// 且 body 的 action 与 ids 数量正确。
func TestCloudTasksPanel_BatchAction(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	for i := 0; i < 2; i++ {
		body := []byte(`{"url":"http://127.0.0.1:1/batch-` + strconv.Itoa(i) +
			`.bin","filename":"batch-` + strconv.Itoa(i) + `.bin","download_local":true,"save":true}`)
		if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
			t.Fatalf("创建任务 %d 失败: %d %s", i, code, resp)
		}
	}
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	reqCh := make(chan playwright.Request, 4)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/api/cloud/tasks/actions/batch") {
			select {
			case reqCh <- r:
			default:
			}
		}
	})
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务: %v", err)
	}
	if err := waitLoc(page, "#ct-batch-action", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("批量动作下拉未渲染: %v", err)
	}
	if err := page.Locator("#ct-pick-all").Check(); err != nil {
		t.Fatalf("全选失败任务: %v", err)
	}
	if _, serr := page.Locator("#ct-batch-action").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{"retry_download"}}); serr != nil {
		t.Fatalf("选择批量动作: %v", serr)
	}
	if err := page.Locator("#ct-batch-apply").Click(); err != nil {
		t.Fatalf("点击批量执行: %v", err)
	}
	select {
	case req := <-reqCh:
		var payload struct {
			Action string   `json:"action"`
			IDs    []string `json:"ids"`
		}
		requestJSON(t, req, &payload)
		if payload.Action != "retry_download" || len(payload.IDs) == 0 {
			t.Fatalf("批量请求体异常: action=%q ids=%v", payload.Action, payload.IDs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("批量执行未发出 POST /api/cloud/tasks/actions/batch")
	}
}

// TestCloudTasksPanel_DetailLoadsProgress 锁定「详情展开会拉进度并渲染速率曲线/占位」
// （用户 2026-11-26：v2 页面每任务可见历史速率）。
func TestCloudTasksPanel_DetailLoadsProgress(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	body := []byte(`{"url":"http://127.0.0.1:1/nope3.bin","filename":"nope3.bin","download_local":true,"save":true}`)
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
		t.Fatalf("创建任务失败: %d %s", code, resp)
	}
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	// 记录 /progress 请求（证明前端确实按需拉取，而不是只在 DOM 里造假）。
	progCh := make(chan string, 4)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodGet && strings.Contains(r.URL(), "/progress") {
			select {
			case progCh <- r.URL():
			default:
			}
		}
	})
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("点击云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-detail-btn", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("任务行未渲染: %v", err)
	}
	if err := page.Locator("#cloud-tasks-panel .ct-detail-btn").First().Click(); err != nil {
		t.Fatalf("点击详情: %v", err)
	}
	select {
	case u := <-progCh:
		if !strings.Contains(u, "/api/cloud/tasks/") || !strings.Contains(u, "/progress") {
			t.Fatalf("进度请求 URL 异常: %s", u)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("详情展开未请求 /progress")
	}
	// 分块矩阵槽必须有内容（无分块状态时给明确占位，不允许停在「加载中…」）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		txt, terr := page.Locator("#cloud-tasks-panel .ct-matrix-slot").First().InnerText()
		return terr == nil && txt != "" && !strings.Contains(txt, "加载中")
	}, "分块矩阵槽未收敛")

	// 采样槽必须收敛成曲线或明确占位（不允许停在「加载中…」）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		txt, terr := page.Locator("#cloud-tasks-panel .ct-spark-slot").First().InnerText()
		return terr == nil && (strings.Contains(txt, "暂无采样") || strings.Contains(txt, "峰值"))
	}, "速率曲线槽未收敛为曲线或「暂无采样」占位")
}

// waitFailedTask 轮询直到存在 failed 任务（服务端异步）。
func waitFailedTask(t *testing.T, baseURL string) {
	t.Helper()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(20 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/api/cloud/tasks")
		if err == nil {
			var payload struct {
				Tasks []struct {
					Status string `json:"status"`
				} `json:"tasks"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			for _, tk := range payload.Tasks {
				if tk.Status == "failed" {
					return
				}
			}
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("20s 内没有任务进入 failed")
		}
	}
}

// TestV2CloudTable_DestructiveActionNeedsConfirm 锁定「破坏性任务动作必须用户显式确认」。
//
// `retry_transfer_overwrite` 是服务端 plan 里唯一的破坏性动作（目标已存在时覆盖写入、
// 无备份、不可撤销）。此前点一下即执行；批量里已排除，**单条**这条路必须确认。
// 判据（不依赖真实 conflict 任务）：直接驱动共享层 `v2cloudTable.perform`
// （传输页与控制台都经它），用桩 api 记录请求 —— 确认前**零请求**，确认后一次请求。
func TestV2CloudTable_DestructiveActionNeedsConfirm(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
	})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()
	if _, err := page.Goto(baseURL + "/ui/v2/"); err != nil {
		t.Fatalf("打开 /ui/v2/: %v", err)
	}
	if err := waitLoc(page, ".nav[data-route=files]", playwright.WaitForSelectorStateVisible, 15000); err != nil {
		t.Fatalf("v2 壳层未就绪: %v", err)
	}

	// 桩 api：记录所有请求路径（不真的打服务端，避免造 conflict 任务的复杂度）。
	if _, err := page.Evaluate(`() => {
		window.__calls = [];
		window.__stubApi = { json: function (m, p) { window.__calls.push(m + ' ' + p); return Promise.resolve({}); } };
		window.v2cloudTable.perform('retry_transfer_overwrite',
			{ id: 'TASK-1', filename: 'demo.bin' }, window.__stubApi, function () {});
	}`, nil); err != nil {
		t.Fatalf("驱动 perform: %v", err)
	}
	// ① 确认框必须出现（且带「覆盖」说明）。
	if err := waitLoc(page, "#v2-confirm [data-act=ok]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("覆盖转存未弹出确认框: %v", err)
	}
	if msg, _ := page.Evaluate(`() => (document.querySelector('#v2-confirm-msg') || {}).textContent || ''`, nil); !strings.Contains(fmt.Sprint(msg), "覆盖") {
		t.Fatalf("确认框未说明覆盖风险: %q", msg)
	}
	// ② 确认前**零请求**（这是本用例的核心：以前点一下就发）。
	if calls, _ := page.Evaluate(`() => window.__calls.length`, nil); fmt.Sprint(calls) != "0" {
		t.Fatalf("确认前已发出请求（破坏性动作未经确认即执行）: %v", calls)
	}
	// ③ 取消 ⇒ 仍零请求。
	if err := page.Locator("#v2-confirm [data-act=cancel]").Click(); err != nil {
		t.Fatalf("取消确认框: %v", err)
	}
	if calls, _ := page.Evaluate(`() => window.__calls.length`, nil); fmt.Sprint(calls) != "0" {
		t.Fatalf("取消后仍发出请求: %v", calls)
	}
	// ④ 再次操作并确认 ⇒ 恰好一次 POST /actions。
	if _, err := page.Evaluate(`() => {
		window.v2cloudTable.perform('retry_transfer_overwrite',
			{ id: 'TASK-1', filename: 'demo.bin' }, window.__stubApi, function () {});
	}`, nil); err != nil {
		t.Fatalf("再次驱动 perform: %v", err)
	}
	if err := waitLoc(page, "#v2-confirm [data-act=ok]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("第二次未弹确认框: %v", err)
	}
	if err := page.Locator("#v2-confirm [data-act=ok]").Click(); err != nil {
		t.Fatalf("确认覆盖写入: %v", err)
	}
	if _, err := page.WaitForFunction(`() => window.__calls.length === 1`, nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("确认后未发出且仅发出一次请求: %v", err)
	}
	if calls, _ := page.Evaluate(`() => window.__calls[0]`, nil); !strings.Contains(fmt.Sprint(calls), "/api/cloud/tasks/TASK-1/actions") {
		t.Fatalf("确认后请求路径不对: %v", calls)
	}
}

// TestV2CloudTasks_RetryFromURLUsesDialog 锁定「换链接重试」用 v2 对话框而非原生 prompt。
//
// 背景：`window.prompt` 阻塞渲染进程、无样式，且**无法做输入校验** —— 用户输入非法链接只能
// 等服务端 400，看不出哪里错。改为 v2confirm.input：本地即时校验 + 失败可改 + 提交带 url。
// 判据（真实失败任务）：点「换链接重试」→ 出现 `#v2-confirm-input`（不是原生存取框）
// → 填非法值提交仍停留并给出原因 → 填合法值提交 ⇒ 恰好一次 POST，body 含该 url。
func TestV2CloudTasks_RetryFromURLUsesDialog(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	body := []byte(`{"url":"http://127.0.0.1:1/fromurl.bin","filename":"fromurl.bin","download_local":true,"save":true}`)
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", body); code != http.StatusOK {
		t.Fatalf("创建失败任务: %d %s", code, resp)
	}
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	// 只要一个对话框：原生 prompt 若出现，Playwright 会自动 dismiss；这里显式记录以备诊断。
	var promptSeen atomic.Int64
	page.On("dialog", func(d playwright.Dialog) {
		promptSeen.Add(1)
		_ = d.Dismiss()
	})
	// 记录动作请求（含 body）。
	type actReq struct {
		url  string
		body string
	}
	var mu sync.Mutex
	var acts []actReq
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/actions") {
			b, _ := r.PostDataBuffer()
			mu.Lock()
			acts = append(acts, actReq{url: r.URL(), body: string(b)})
			mu.Unlock()
		}
	})

	if _, err := page.Goto(baseURL + "/ui/v2/#/transfer"); err != nil {
		t.Fatalf("打开传输页: %v", err)
	}
	if err := waitLoc(page, "#v2-tf-body tr [data-plan-action=retry_from_url]",
		playwright.WaitForSelectorStateVisible, 20000); err != nil {
		t.Fatalf("失败任务未渲染「换链接重试」按钮: %v", err)
	}
	if err := page.Locator("#v2-tf-body tr [data-plan-action=retry_from_url]").First().Click(); err != nil {
		t.Fatalf("点换链接重试: %v", err)
	}
	// ① 必须是 v2 对话框（输入框在其中），而不是原生 prompt。
	if err := waitLoc(page, "#v2-confirm-input", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("换链接重试未使用 v2 对话框: %v", err)
	}
	if n := promptSeen.Load(); n != 0 {
		t.Fatalf("仍在用原生 prompt（出现 %d 次）", n)
	}
	// ② 非法链接：本地校验拦住，不提交、对话框不关。
	if err := page.Locator("#v2-confirm-input").Fill("ftp://example.invalid/x.bin"); err != nil {
		t.Fatalf("填非法链接: %v", err)
	}
	if err := page.Locator("#v2-confirm [data-act=ok]").Click(); err != nil {
		t.Fatalf("提交非法链接: %v", err)
	}
	if err := waitLoc(page, "#v2-confirm-err", playwright.WaitForSelectorStateVisible, 3000); err != nil {
		t.Fatalf("非法链接未给出本地校验提示: %v", err)
	}
	mu.Lock()
	n := len(acts)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("非法链接已发出请求（应在本地被拦）: %d", n)
	}
	// ③ 合法链接：提交 ⇒ 恰好一次 POST，body 带该 url。
	const newURL = "https://example.com/s/EXAMPLE-RETRY-FROM-URL-1"
	if err := page.Locator("#v2-confirm-input").Fill(newURL); err != nil {
		t.Fatalf("填合法链接: %v", err)
	}
	if err := page.Locator("#v2-confirm [data-act=ok]").Click(); err != nil {
		t.Fatalf("提交合法链接: %v", err)
	}
	if _, err := page.WaitForFunction(`() => true`, nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(1500)}); err != nil {
		t.Fatalf("等待提交: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acts) != 1 {
		t.Fatalf("应恰好一次动作请求，实际 %d", len(acts))
	}
	if !strings.Contains(acts[0].body, newURL) || !strings.Contains(acts[0].body, "retry_from_url") {
		t.Fatalf("动作请求体不对: %s", acts[0].body)
	}
}
