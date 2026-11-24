// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2_shell_e2e_test.go — P0 壳层冒烟：路由 / 主题 / 深链刷新 / 旧版入口。
package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	serverconfig "github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/mxschmitt/playwright-go"
)

// TestV2Shell_RouteThemeAndEntry 验证 /ui/v2/ 可用且不破坏旧版入口。
func TestV2Shell_RouteThemeAndEntry(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	// 真实通知渠道：本地 webhook 接收端，验证「渠道自检」真的外发。
	var hookHits atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hookHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []serverconfig.NotifyRule{{Action: "*", Channels: []string{"webhook"}}}
		c.Notify.Channels.Webhook.URL = hook.URL
	})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 播种一个真实目录，验证 P1a 列表与面包屑（测试实例允许 loopback 匿名）。
	resp, mkErr := http.Post(baseURL+"/mkdir?dirname=v2demo", "", nil)
	if mkErr != nil {
		t.Fatalf("seed mkdir: %v", mkErr)
	}
	resp.Body.Close()

	if _, err := page.Goto(baseURL + "/ui/v2/"); err != nil {
		t.Fatalf("打开 /ui/v2/: %v", err)
	}
	if err := waitLoc(page, ".nav[data-route]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("v2 侧栏未渲染: %v", err)
	}
	if n, _ := page.Locator(".nav[data-route]").Count(); n != 4 {
		t.Fatalf("侧栏入口数 = %d，期望 4", n)
	}
	if txt, _ := page.Locator("#v2-title").TextContent(); txt != "文件" {
		t.Fatalf("默认路由标题 = %q，期望 文件", txt)
	}

	// P1a：列表来自 /api/files，且可进入子目录。
	if err := waitLoc(page, "#v2-file-body tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("文件列表未渲染: %v", err)
	}
	if txt, _ := page.Locator("#v2-file-body").TextContent(); !strings.Contains(txt, "v2demo") {
		t.Fatalf("列表缺少播种目录 v2demo: %q", txt)
	}
	// P1c：检查器（概览 / Meta / 版本 / 分享）可打开且切换页签。
	if err := page.Locator("#v2-file-body tr[data-name=v2demo] [data-action=detail]").Click(); err != nil {
		t.Fatalf("打开详情: %v", err)
	}
	if err := waitLoc(page, "#v2-drawer.show", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("检查器未打开: %v", err)
	}
	if txt, _ := page.Locator("#v2-dr-title").TextContent(); txt != "v2demo" {
		t.Fatalf("检查器标题 = %q", txt)
	}
	if err := page.Locator("[data-drtab=meta]").Click(); err != nil {
		t.Fatalf("切换到 Meta: %v", err)
	}
	if err := page.Locator("#v2-dr-close").Click(); err != nil {
		t.Fatalf("关闭检查器: %v", err)
	}

	if err := page.Locator("#v2-file-body a").First().Click(); err != nil {
		t.Fatalf("进入 v2demo: %v", err)
	}
	if h, _ := page.Evaluate("() => location.hash", nil); h != "#/files/v2demo" {
		t.Fatalf("子目录 hash = %v", h)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-crumbs').textContent.indexOf('v2demo') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("面包屑未更新为 v2demo: %v", err)
	}

	// P1b：UI 新建目录 → 选择 → 批量删除（走真实 /mkdir 与 /rmdir）。
	if err := page.Locator("#v2-mkdir").Click(); err != nil {
		t.Fatalf("打开新建目录: %v", err)
	}
	if err := page.Locator("#v2-dlg-input").Fill("uifolder"); err != nil {
		t.Fatalf("填写目录名: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("提交新建目录: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-file-body').textContent.indexOf('uifolder') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("新建目录未出现在列表: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=uifolder] .cb").Check(); err != nil {
		t.Fatalf("勾选新目录: %v", err)
	}
	if err := page.Locator("#v2-batch-delete").Click(); err != nil {
		t.Fatalf("点批量删除: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("确认删除: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-file-body').textContent.indexOf('uifolder') === -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("目录删除后仍在列表: %v", err)
	}

	// P1d：上传（简单路径）→ 行出现。
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "e2e.txt", MimeType: "text/plain", Buffer: []byte("hello e2e"),
	}}); err != nil {
		t.Fatalf("选择上传文件: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-file-body').textContent.indexOf('e2e.txt') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("上传后列表未出现 e2e.txt: %v", err)
	}

	// P1e：文本文件点击预览（真实 /download 读取）。
	if err := page.Locator("#v2-file-body tr[data-name='e2e.txt'] a[data-preview]").Click(); err != nil {
		t.Fatalf("打开预览: %v", err)
	}
	if err := waitLoc(page, "#v2-preview", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("预览未打开: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-pv-text').textContent.indexOf('hello e2e') !== -1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("文本预览内容不正确: %v", err)
	}
	if err := page.Locator("#v2-pv-close").Click(); err != nil {
		t.Fatalf("关闭预览: %v", err)
	}

	// 切换路由：hash + 面板可见性。
	if err := page.Locator(".nav[data-route='transfer']").Click(); err != nil {
		t.Fatalf("点击传输: %v", err)
	}
	if h, _ := page.Evaluate("() => location.hash", nil); h != "#/transfer" {
		t.Fatalf("hash = %v，期望 #/transfer", h)
	}
	hidden, _ := page.Evaluate("() => document.querySelector('[data-panel=transfer]').hidden", nil)
	if hidden != false {
		t.Fatalf("传输面板应可见，实际 hidden=%v", hidden)
	}

	// 刷新后深链保持。
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新: %v", err)
	}
	if err := waitLoc(page, "[data-panel='transfer']", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("刷新后未停留在传输: %v", err)
	}

	// P2a：传输页（云端任务）可加载并显示空态/计数。
	if err := page.Locator(".nav[data-route=transfer]").Click(); err != nil {
		t.Fatalf("进入传输页: %v", err)
	}
	if err := waitLoc(page, "#v2-tf-empty", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("传输页空态未显示: %v", err)
	}
	if n, _ := page.Locator("[data-tf]").Count(); n != 4 {
		t.Fatalf("传输筛选 chips = %d，期望 4", n)
	}
	// P2b：任务组空态 + 创建对话框可打开/取消。
	if err := waitLoc(page, "#v2-grp-empty", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("任务组空态未显示: %v", err)
	}
	if err := page.Locator("#v2-grp-create").Click(); err != nil {
		t.Fatalf("打开创建组: %v", err)
	}
	if err := waitLoc(page, "#v2-grp-backdrop .modal", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("创建组对话框未打开: %v", err)
	}
	if err := page.Locator("#v2-grp-cancel").Click(); err != nil {
		t.Fatalf("取消创建组: %v", err)
	}
	// P2c：同步任务空态 + 新建对话框可打开/取消。
	if err := waitLoc(page, "#v2-sync-empty", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("同步任务空态未显示: %v", err)
	}
	if err := page.Locator("#v2-sync-create").Click(); err != nil {
		t.Fatalf("打开新建同步: %v", err)
	}
	if err := waitLoc(page, "#v2-sync-backdrop .modal", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("同步对话框未打开: %v", err)
	}
	if err := page.Locator("#v2-sync-cancel").Click(); err != nil {
		t.Fatalf("取消新建同步: %v", err)
	}

	// P3a：控制台概览（KPI 或 503 提示）+ 分区导航。
	if err := page.Locator(".nav[data-route=console]").Click(); err != nil {
		t.Fatalf("进入控制台: %v", err)
	}
	if err := waitLoc(page, "#v2-cs-refresh", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("控制台未渲染: %v", err)
	}
	if n, _ := page.Locator(".cs-nav button").Count(); n != 9 {
		t.Fatalf("控制台分区 = %d，期望 9", n)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-cs-kpis').children.length > 0 || !document.querySelector('#v2-cs-state').hidden",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("控制台概览既无 KPI 也无提示: %v", err)
	}
	// 控制台分区：云端任务 / 待处理事项（复用同一数据源，允许为空或未装配提示）。
	if err := page.Locator("[data-cs=tasks]").Click(); err != nil {
		t.Fatalf("切换云端任务分区: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=tasks]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("控制台云端任务未渲染: %v", err)
	}
	if err := page.Locator("[data-cs=issues]").Click(); err != nil {
		t.Fatalf("切换待处理事项分区: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=issues]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("控制台待处理事项未渲染: %v", err)
	}
	// 审计分区：过滤条与列表容器可用。
	if err := page.Locator("[data-cs=audit]").Click(); err != nil {
		t.Fatalf("切换审计分区: %v", err)
	}
	if err := waitLoc(page, "#v2-au-apply", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("审计分区未渲染: %v", err)
	}
	// 配置分区：可写字段与只读信息（或读取失败提示）。
	if err := page.Locator("[data-cs=config]").Click(); err != nil {
		t.Fatalf("切换配置分区: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const f = document.querySelector('#v2-cfg-facts'); return (f && f.children.length > 0) || !document.querySelector('#v2-cfg-state').hidden; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("配置分区既无字段也无提示: %v", err)
	}
	// 通知分区：真实渠道自检 → 本地 webhook 收到请求 + 页面回显 ok + 历史留痕。
	if err := page.Locator("[data-cs=notify]").Click(); err != nil {
		t.Fatalf("切换通知分区: %v", err)
	}
	if err := waitLoc(page, "#v2-nt-test", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("通知分区未渲染: %v", err)
	}
	if err := page.Locator("#v2-nt-test").Click(); err != nil {
		t.Fatalf("渠道自检点击: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-nt-state'); return b && !b.hidden && b.textContent.includes('webhook'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("渠道自检未回显 webhook 结果: %v", err)
	}
	if hookHits.Load() == 0 {
		t.Fatalf("本地 webhook 未收到自检消息")
	}

	// 巡检分区：真实 POST /api/verify —— 前面已上传 e2e.txt，台账至少 1 项且一致。
	if err := page.Locator("[data-cs=verify]").Click(); err != nil {
		t.Fatalf("切换巡检分区: %v", err)
	}
	if err := page.Locator("#v2-vf-run").Click(); err != nil {
		t.Fatalf("触发巡检: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const ks = document.querySelectorAll('#v2-vf-kpis .kpi .v'); return ks.length >= 6 && ks[0].textContent !== '—'; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("巡检报告未渲染: %v", err)
	}
	totalText, _ := page.Locator("#v2-vf-kpis .kpi").First().Locator(".v").TextContent()
	okText, _ := page.Locator("#v2-vf-kpis .kpi").Nth(1).Locator(".v").TextContent()
	if strings.TrimSpace(totalText) == "0" || strings.TrimSpace(okText) == "0" {
		t.Fatalf("巡检未核对到已上传文件：total=%q ok=%q", totalText, okText)
	}

	// 接入分区：凭据清单（admin）或 403 降级提示，二者都须可见。
	if err := page.Locator("[data-cs=access]").Click(); err != nil {
		t.Fatalf("切换接入分区: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-ac-state'); const t = document.querySelector('#v2-ac-body'); return (b && !b.hidden) || (t && t.children.length > 0); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("接入分区既无凭据也无提示: %v", err)
	}
	// 命令面板：⌘K/Ctrl+K → 搜索「配置」→ Enter 直达控制台配置分区。
	if err := page.Keyboard().Press("Control+k"); err != nil {
		t.Fatalf("打开命令面板: %v", err)
	}
	if err := waitLoc(page, "#v2-cmdk", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("命令面板未显示: %v", err)
	}
	if err := page.Locator("#v2-cmdk-input").Fill("配置"); err != nil {
		t.Fatalf("命令面板输入: %v", err)
	}
	if err := page.Keyboard().Press("Enter"); err != nil {
		t.Fatalf("执行命令: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=config]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("命令面板未跳到配置分区: %v", err)
	}

	// 快捷键帮助：? 打开、Esc 关闭。
	if err := page.Keyboard().Press("?"); err != nil {
		t.Fatalf("打开快捷键帮助: %v", err)
	}
	if err := waitLoc(page, "#v2-help", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("快捷键帮助未显示: %v", err)
	}
	if err := page.Keyboard().Press("Escape"); err != nil {
		t.Fatalf("关闭快捷键帮助: %v", err)
	}
	if err := waitLoc(page, "#v2-help", playwright.WaitForSelectorStateHidden, 5000); err != nil {
		t.Fatalf("快捷键帮助未关闭: %v", err)
	}

	// g f 跳文件页；j/k 移动行、x 选中、Enter 打开预览（真实文件 e2e.txt）。
	if err := page.Keyboard().Press("g"); err != nil {
		t.Fatalf("g 前缀: %v", err)
	}
	if err := page.Keyboard().Press("f"); err != nil {
		t.Fatalf("g f 跳转: %v", err)
	}
	if err := waitLoc(page, "[data-panel=files]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("未跳到文件页: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("跳到 v2demo 目录: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !!document.querySelector('#v2-file-body tr[data-name=\"e2e.txt\"]')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("v2demo 目录未列到 e2e.txt: %v", err)
	}
	if err := page.Keyboard().Press("j"); err != nil {
		t.Fatalf("j 移动: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !!document.querySelector('#v2-file-body tr.row-focus')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("键盘焦点行未出现: %v", err)
	}
	if err := page.Keyboard().Press("x"); err != nil {
		t.Fatalf("x 选中: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => (document.querySelector('#v2-count') || {}).textContent?.includes('已选 1')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("x 未选中当前行: %v", err)
	}
	if err := page.Keyboard().Press("Enter"); err != nil {
		t.Fatalf("Enter 打开: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const p = document.querySelector('#v2-preview'); return p && !p.hidden && document.querySelector('#v2-preview .pv-stage').textContent.includes('hello e2e'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("Enter 未打开文本预览: %v", err)
	}
	if err := page.Keyboard().Press("Escape"); err != nil {
		t.Fatalf("关闭预览: %v", err)
	}

	// 密度切换：写入 localStorage，刷新后保持紧凑。
	if err := page.Locator("#v2-density-toggle").Click(); err != nil {
		t.Fatalf("切换密度: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.documentElement.getAttribute('data-density') === 'compact'",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("密度未切换为 compact: %v", err)
	}
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.documentElement.getAttribute('data-density') === 'compact'",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("刷新后密度未保持: %v", err)
	}

	// 主题切换。
	before, _ := page.Evaluate("() => document.documentElement.getAttribute('data-theme')", nil)
	if err := page.Locator("#v2-theme-toggle").Click(); err != nil {
		t.Fatalf("切换主题: %v", err)
	}
	after, _ := page.Evaluate("() => document.documentElement.getAttribute('data-theme')", nil)
	if before == after {
		t.Fatalf("主题未切换：%v -> %v", before, after)
	}

	// 旧版入口：旧页提供试用链接，且旧页本身仍可用。
	if _, err := page.Goto(baseURL + "/ui/"); err != nil {
		t.Fatalf("打开旧版: %v", err)
	}
	href, err := page.Locator("#v2-entry-link").GetAttribute("href")
	if err != nil || href != "/ui/v2/" {
		t.Fatalf("旧版试用入口 href = %v（err=%v）", href, err)
	}
}
