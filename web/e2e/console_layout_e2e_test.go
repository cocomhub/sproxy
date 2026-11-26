// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// console_layout_e2e_test.go — 控制台（原监控弹窗）全屏化 + 分区导航 + 深链 E2E（2026-11-22）。
//
// 断言：① 面板尺寸接近视口（宽 ≥ 60vw、高 ≥ 70vh），不再是 500px 小窗；
// ② 左侧导航含全部分区；③ 点击「云端任务」切分区并写 hash；④ **刷新后仍停在云端任务**（深链）。

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestConsoleOverviewAndFilters 覆盖概览卡片 + 云端任务筛选/复制按钮（2026-11-22）。
func TestConsoleOverviewAndFilters(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	// 造一个失败任务（供卡片与列表断言）。
	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/ov.bin","filename":"ov.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// ① 概览卡片：四张卡片必须渲染，且「云端任务失败」计数 ≥1（tone=bad）。
	if err := waitLoc(page, "#overview-cards .overview-card", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("概览卡片未渲染: %v", err)
	}
	// 卡片数 = 4 张健康/任务卡 + 1 张存储水位卡（2026-11-22 新增）。
	if n, _ := page.Locator("#overview-cards .overview-card").Count(); n != 5 {
		t.Fatalf("概览卡片数 = %d，want 5（含存储水位）", n)
	}
	txt, _ := page.Locator("#overview-cards [data-card='cloud-failed']").TextContent()
	if !strings.Contains(txt, "1") {
		t.Fatalf("云端失败卡片未反映失败任务: %q", txt)
	}

	// ② 云端任务筛选控件 + 复制按钮存在，输入关键字可过滤（无匹配 → 0 行）。
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("切换云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-copy", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("复制按钮未渲染: %v", err)
	}
	if err := page.Locator("#ct-query").Fill("zzz-no-match"); err != nil {
		t.Fatalf("填写关键字: %v", err)
	}
	if err := page.Locator("#ct-query").Press("Enter"); err != nil {
		t.Fatalf("提交关键字: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel tbody tr", playwright.WaitForSelectorStateDetached, 5000); err != nil {
		// 过滤后无行（tbody 内 tr 数量为 0）也算通过；这里用 detached 语义兜底。
		if n, _ := page.Locator("#cloud-tasks-panel tbody tr").Count(); n != 0 {
			t.Fatalf("关键字过滤未生效（仍有 %d 行）: %v", n, err)
		}
	}
}

// TestConsoleCardsJumpAndPaging 覆盖概览卡片一键跳转（带过滤）、分页控件与最近审计流（2026-11-22）。
func TestConsoleCardsJumpAndPaging(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/j.bin","filename":"j.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// ① 概览底部「最近审计事件」区块存在（无事件也显示占位）。
	if err := waitLoc(page, "#overview-audit", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("概览审计流未渲染: %v", err)
	}

	// ② 点「云端任务失败」卡片 → 切到云端任务且**只看失败**已勾选；分页控件存在。
	if err := page.Locator("#overview-cards [data-card='cloud-failed']").Click(); err != nil {
		t.Fatalf("点击失败卡片: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel #ct-failed-only", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("云端任务面板未就绪: %v", err)
	}
	checked, _ := page.Locator("#ct-failed-only").IsChecked()
	if !checked {
		t.Fatal("卡片跳转后「只看失败」未勾选")
	}
	if n, _ := page.Locator("#ct-prev").Count(); n != 1 {
		t.Fatalf("分页控件缺失（ct-prev）")
	}
}

// TestConsoleExportAndTrend 覆盖概览趋势副标题与云端任务「导出TSV」按钮（2026-11-22）。
func TestConsoleExportAndTrend(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/tr.bin","filename":"tr.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// ① 失败卡片副标题带「近 1 小时新增」（服务端刚失败，必在窗口内）。
	if err := waitLoc(page, "#overview-cards [data-card='cloud-failed']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("概览卡片未渲染: %v", err)
	}
	hint, _ := page.Locator("#overview-cards [data-card='cloud-failed']").GetAttribute("title")
	if !strings.Contains(hint, "近 1 小时新增") {
		t.Fatalf("失败卡片缺少趋势提示: %q", hint)
	}

	// ② 云端任务面板有「导出TSV」按钮，且导出内容包含表头（拦截 Blob 生成不便，改为校验按钮存在 + 纯函数已单测）。
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("切换云端任务: %v", err)
	}
	if err := waitLoc(page, "#ct-export", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("导出按钮缺失: %v", err)
	}
}

// TestConsoleImportTSVRetries 覆盖「导出→导入→重试」闭环（2026-11-22）：
// 上传 TSV 后必须逐条发出 retry 请求，并显示汇总消息（重渲染后仍可见）。
func TestConsoleImportTSVRetries(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/im.bin","filename":"im.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("切换云端任务: %v", err)
	}
	if err := waitLoc(page, "#ct-import", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("导入控件缺失: %v", err)
	}

	// 取真实任务 ID 构造 TSV（模拟"导出后再导入"）。
	idsRaw, err := page.Evaluate("() => (window.__ctLastTasks || []).map(t => t.id)", nil)
	if err != nil {
		t.Fatalf("读取任务 ID: %v", err)
	}
	ids, _ := idsRaw.([]any)
	if len(ids) == 0 {
		t.Fatal("页面未加载到任务（无法构造导入文件）")
	}
	taskID, _ := ids[0].(string)

	planPath := filepath.Join(t.TempDir(), "plan.tsv")
	content := "id\tstatus\tstage\terror_class\tattempts\tvolume\tpath\turl\tpartial_bytes\n" +
		taskID + "\tfailed\tdownloading\tsource_unreachable\t1\t\t\thttp://127.0.0.1:1/im.bin\t0\n"
	if werr := os.WriteFile(planPath, []byte(content), 0o600); werr != nil {
		t.Fatalf("写 TSV: %v", werr)
	}

	reqCh := make(chan playwright.Request, 4)
	page.On("request", func(r playwright.Request) {
		if r.Method() == http.MethodPost && strings.Contains(r.URL(), "/api/cloud/tasks/retry") {
			select {
			case reqCh <- r:
			default:
			}
		}
	})
	if err := page.Locator("#ct-import").SetInputFiles(planPath); err != nil {
		t.Fatalf("导入 TSV: %v", err)
	}
	select {
	case req := <-reqCh:
		var payload struct {
			TaskID string `json:"task_id"`
			Force  bool   `json:"force"`
		}
		requestJSON(t, req, &payload)
		if payload.TaskID != taskID || !payload.Force {
			t.Fatalf("导入重试请求体异常: %+v（want task_id=%s force=true）", payload, taskID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("导入后未发出 retry 请求")
	}
	// 汇总消息在重渲染后仍需可见（由 __ctMsg 回填）。异步：轮询至出现（最多 5s）。
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		msg, _ := page.Locator("#ct-msg").TextContent()
		if strings.Contains(msg, "导入重试") {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("导入结果消息缺失: %q", msg)
		}
	}
}

// TestConsoleDeepLinkFilters 锁定深链带过滤：直接打开带参数的 URL 即恢复筛选视图（2026-11-22）。
func TestConsoleDeepLinkFilters(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/dl.bin","filename":"dl.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	// 直接访问「云端任务 + 只看失败 + 类别 source_unreachable」深链。
	page.Goto(baseURL + "/ui/#/console/cloud-tasks?failed=1&cls=source_unreachable")
	openStatsModal(t, page)

	if err := waitLoc(page, "#cloud-tasks-panel #ct-failed-only", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("深链未打开云端任务分区: %v", err)
	}
	checked, _ := page.Locator("#ct-failed-only").IsChecked()
	if !checked {
		t.Fatal("深链 failed=1 未生效（只看失败未勾选）")
	}
	cls, _ := page.Locator("#ct-class").InputValue()
	if cls != "source_unreachable" {
		t.Fatalf("深链 cls 未生效: %q", cls)
	}
	// 至少 1 行（该任务类别的失败任务）
	if n, _ := page.Locator("#cloud-tasks-panel tbody tr").Count(); n < 1 {
		t.Fatalf("深链过滤后应有失败任务行，got %d", n)
	}
}

// TestConsoleCardJumpWritesDeepLink 锁定卡片跳转**写深链**（可分享该视图，2026-11-22）。
func TestConsoleCardJumpWritesDeepLink(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/hl.bin","filename":"hl.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	// 超时口径（2026-11-27 实测 CI 偶发）：概览卡片需要 modal 打开后再串行拉
	// /api/cloud/tasks → /api/issues/stats → /api/volumes 三份数据才插入 DOM；CI 容器比其他
	// job 慢，8s 偶发不足（本地稳定通过）。按仓库既有「-race/慢机超时翻倍」纪律放宽到 20s，
	// 并在失败时抓取任务快照，便于区分「服务端无 failed 任务」与「前端渲染慢」。
	if err := waitLoc(page, "#overview-cards [data-card='cloud-failed']", playwright.WaitForSelectorStateVisible, 20000); err != nil {
		tasksResp, terr := http.Get(baseURL + "/api/cloud/tasks")
		snapshot := "(读取任务快照失败)"
		if terr == nil {
			body, _ := io.ReadAll(tasksResp.Body)
			tasksResp.Body.Close()
			snapshot = string(body)
		}
		t.Fatalf("概览卡片未渲染（20s）: %v；任务快照: %s", err, snapshot)
	}
	if err := page.Locator("#overview-cards [data-card='cloud-failed']").Click(); err != nil {
		t.Fatalf("点击失败卡片: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel #ct-failed-only", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("未切到云端任务: %v", err)
	}
	h, _ := page.Evaluate("() => location.hash", nil)
	if h != "#/console/cloud-tasks?failed=1" {
		t.Fatalf("卡片跳转未写深链: %v（want #/console/cloud-tasks?failed=1）", h)
	}
}

// TestConsoleKeyboardNav 锁定键盘导航：Alt+→ 切下一个分区、Alt+1 回概览、Esc 关闭（2026-11-22）。
func TestConsoleKeyboardNav(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// Alt+→ ⇒ 第二个分区（云端任务）。
	if err := page.Keyboard().Press("Alt+ArrowRight"); err != nil {
		t.Fatalf("Alt+ArrowRight: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("Alt+→ 未切到云端任务: %v", err)
	}

	// Alt+1 ⇒ 概览（第一个分区）。
	if err := page.Keyboard().Press("Alt+1"); err != nil {
		t.Fatalf("Alt+1: %v", err)
	}
	if err := waitLoc(page, "#stats-panel", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("Alt+1 未回到概览: %v", err)
	}

	// Esc ⇒ 关闭控制台。
	if err := page.Keyboard().Press("Escape"); err != nil {
		t.Fatalf("Escape: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateHidden, 5000); err != nil {
		t.Fatalf("Esc 未关闭控制台: %v", err)
	}
}

// TestConsoleIssuesExportAndSizeFilter 覆盖事项导出CSV按钮与云端任务体积阈值控件（2026-11-22）。
func TestConsoleIssuesExportAndSizeFilter(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/sz.bin","filename":"sz.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	// ① 云端任务：体积阈值控件存在，输入超大阈值后列表被过滤为空。
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("切换云端任务: %v", err)
	}
	if err := waitLoc(page, "#ct-minsize", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("体积阈值控件缺失: %v", err)
	}
	if err := page.Locator("#ct-minsize").Fill("1000000"); err != nil {
		t.Fatalf("填写阈值: %v", err)
	}
	if err := page.Locator("#ct-minsize").Press("Enter"); err != nil {
		t.Fatalf("提交阈值: %v", err)
	}
	if _, err := page.Locator("#ct-minsize").Evaluate("el => el.dispatchEvent(new Event('change'))", nil); err != nil {
		t.Fatalf("触发 change: %v", err)
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		n, _ := page.Locator("#cloud-tasks-panel tbody tr").Count()
		if n == 0 {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("体积阈值过滤未生效（仍有 %d 行）", n)
		}
	}

	// ② 事项导出CSV 由纯函数单测覆盖（`issuesExportCSV`/`issuesCSVField`）；
	//    本测试环境 /api/issues 未装配 issue center，面板只显示占位，故不在此断言按钮。
	_ = baseURL
}

// TestConsoleHelpPanel 覆盖 `?` 打开/关闭快捷键帮助（2026-11-23）。
func TestConsoleHelpPanel(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)

	if err := page.Keyboard().Press("?"); err != nil {
		t.Fatalf("按下 ?: %v", err)
	}
	if err := waitLoc(page, "#console-help", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("帮助面板未打开: %v", err)
	}
	text, _ := page.Locator("#console-help").TextContent()
	if !strings.Contains(text, "Alt+") || !strings.Contains(text, "Esc") {
		t.Fatalf("帮助内容缺少快捷键: %q", text)
	}
	if err := page.Keyboard().Press("?"); err != nil {
		t.Fatalf("再次按下 ?: %v", err)
	}
	if err := waitLoc(page, "#console-help", playwright.WaitForSelectorStateDetached, 5000); err != nil {
		t.Fatalf("帮助面板未关闭: %v", err)
	}
}

// TestConsoleVolumeChipsAndActionLog 覆盖「按卷分组 chips」与面板内「最近操作」记录（2026-11-23）。
func TestConsoleVolumeChipsAndActionLog(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
	})
	defer cleanup()

	postJSON(t, baseURL+"/api/cloud/download",
		[]byte(`{"url":"http://127.0.0.1:1/vc.bin","filename":"vc.bin","download_local":true,"save":true}`))
	waitFailedTask(t, baseURL)

	page, stop := pageFixture(t)
	defer stop()
	page.Goto(baseURL + "/ui/")
	openStatsModal(t, page)
	if err := page.Locator("#cloud-tasks-tab").Click(); err != nil {
		t.Fatalf("切换云端任务: %v", err)
	}
	if err := waitLoc(page, "#cloud-tasks-panel .ct-retry", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("任务行未渲染: %v", err)
	}

	// ① 按卷分组 chips 存在（该任务无转存卷 ⇒ 归入「(无目标卷)」）。
	if n, _ := page.Locator("#cloud-tasks-panel .ct-vol-chip").Count(); n < 1 {
		t.Fatalf("按卷分组 chip 缺失")
	}

	// ② 点「重试」后，面板底部「最近操作」必须出现该动作记录（sessionStorage 持久）。
	if err := page.Locator("#cloud-tasks-panel .ct-retry").First().Click(); err != nil {
		t.Fatalf("点击重试: %v", err)
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(8 * time.Second)
	for {
		html, _ := page.Locator("#cloud-tasks-panel").InnerHTML()
		if strings.Contains(html, "最近操作") && strings.Contains(html, "重试") {
			break
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("操作审计流未记录重试动作")
		}
	}
}
