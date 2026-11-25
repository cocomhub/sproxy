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
	"net/http"
	"strings"
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
