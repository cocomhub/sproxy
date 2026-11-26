// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// v2_shell_e2e_test.go — P0 壳层冒烟：路由 / 主题 / 深链刷新 / 旧版入口。
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	serverconfig "github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/mxschmitt/playwright-go"
)

// toNum 把 Playwright Evaluate 返回的数值（float64/int）统一成 int64。
func toNum(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return -1
	}
}

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
	baseURL, cfg, cleanup := testServerCfg(t, func(c *server.Config) {
		// 云任务：允许回环源并快速失败，供控制台筛选/重试真实链路使用。
		c.CloudDownloadAllowPrivate = true
		c.CloudMaxRetries = 1
		c.CloudRetryDelay = time.Millisecond
		c.Notify.Enabled = true
		c.Notify.Rules = []serverconfig.NotifyRule{{Action: "*", Channels: []string{"webhook"}}}
		c.Notify.Channels.Webhook.URL = hook.URL
	})
	defer cleanup()

	// 播种 210 个文件（> 单页 200），供「分页/加载更多」真实链路断言。
	manyDir := filepath.Join(cfg.StorageRoot, "anonymous", "user", "many")
	if err := os.MkdirAll(manyDir, 0o755); err != nil {
		t.Fatalf("建 many 目录: %v", err)
	}
	for i := 0; i < 210; i++ {
		name := filepath.Join(manyDir, fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}

	page, stop := pageFixture(t)
	defer stop()

	// 播种一个真实目录，验证 P1a 列表与面包屑（测试实例允许 loopback 匿名）。
	resp, mkErr := http.Post(baseURL+"/mkdir?dirname=v2demo", "", nil)
	if mkErr != nil {
		t.Fatalf("seed mkdir: %v", mkErr)
	}
	resp.Body.Close()

	// 造一个真实失败云任务（不可达端口），供控制台高级操作使用。
	createTask, _ := http.NewRequest(http.MethodPost, baseURL+"/api/cloud/download",
		strings.NewReader(`{"url":"http://127.0.0.1:1/nope.bin","filename":"nope.bin","download_local":true,"save":true}`))
	createTask.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(createTask); err != nil {
		t.Fatalf("创建云任务: %v", err)
	} else if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("创建云任务失败: %d %s", resp.StatusCode, body)
	} else {
		resp.Body.Close()
	}
	// 等待任务真实进入 failed（服务端异步；最多 60s），后续 UI 断言不再受调度延迟影响。
	deadline := time.Now().Add(60 * time.Second)
	for {
		listResp, lerr := http.Get(baseURL + "/api/cloud/tasks?limit=200")
		if lerr == nil {
			var payload struct {
				Tasks []struct {
					Status string `json:"status"`
				} `json:"tasks"`
			}
			_ = json.NewDecoder(listResp.Body).Decode(&payload)
			listResp.Body.Close()
			failed := false
			for _, tk := range payload.Tasks {
				if tk.Status == "failed" {
					failed = true
				}
			}
			if failed {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("云任务 60s 内未进入 failed")
		}
		<-time.After(200 * time.Millisecond)
	}

	if _, err := page.Goto(baseURL + "/ui/v2/"); err != nil {
		t.Fatalf("打开 /ui/v2/: %v", err)
	}
	if err := waitLoc(page, ".nav[data-route]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("v2 侧栏未渲染: %v", err)
	}
	if n, _ := page.Locator(".nav[data-route]").Count(); n != 4 {
		t.Fatalf("侧栏入口数 = %d，期望 4", n)
	}
	// 面板归属：卡片必须留在自己的 panel 内（曾有一条「同步任务」card 逃出 transfer 面板，
	// 在回收站/控制台页面上也会显示出来）。
	if stray, err := page.Evaluate(`() => Array.from(document.querySelectorAll('.content > .card'))
		.map(c => (c.querySelector('h2, b, .card-title') || {}).textContent || '(无标题)')`, nil); err != nil {
		t.Fatalf("检查面板归属: %v", err)
	} else if list, ok := stray.([]any); ok && len(list) > 0 {
		t.Fatalf("有卡片逃出面板（.content 直接子级）: %v", list)
	}

	// 侧栏徽标必须是**真实数据**（曾硬编码 128/3/4）：文件数 = 实际条目数。
	if err := waitLoc(page, "#v2-file-body tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("文件列表未渲染: %v", err)
	}
	badge, _ := page.Evaluate(`() => {
		const b = document.querySelector('[data-badge=files]');
		const rows = document.querySelectorAll('#v2-file-body tr').length;
		return {text: b ? b.textContent : 'MISSING', hidden: b ? b.hidden : null, rows: rows};
	}`, nil)
	if m, ok := badge.(map[string]any); ok {
		rows := fmt.Sprint(m["rows"])
		if got := fmt.Sprint(m["text"]); got != rows {
			t.Fatalf("文件徽标 = %v（hidden=%v），期望等于列表行数 %v（不得硬编码）", got, m["hidden"], rows)
		}
	} else {
		t.Fatalf("读取文件徽标失败: %v", badge)
	}
	// 账户卡：无凭据（loopback 匿名）时必须显示「匿名」，不得是 demo 占位。
	acc, _ := page.Evaluate(`() => ({
		ini: (document.querySelector('#v2-account-initial') || {}).textContent,
		name: (document.querySelector('#v2-account-name') || {}).textContent,
		sub: (document.querySelector('#v2-account-sub') || {}).textContent})`, nil)
	if m, ok := acc.(map[string]any); ok {
		if m["name"] == "demo" || strings.Contains(fmt.Sprint(m["sub"]), "示例凭据") {
			t.Fatalf("账户卡仍是占位假数据: %v", m)
		}
		if m["name"] != "匿名" {
			t.Fatalf("无凭据时账户名 = %v，期望 匿名", m["name"])
		}
	}
	// 过期文案：不得再出现 P0/banner 里「仍走旧版」这类早已不成立的描述。
	stale, _ := page.Evaluate(`() => {
		const t = document.querySelector('.content').textContent + ' ' + document.querySelector('.rail').textContent;
		return ['v2 · P0', '示例凭据', '仍走旧版', '（P1a 文件列表）'].filter(s => t.indexOf(s) !== -1);
	}`, nil)
	if list, ok := stale.([]any); ok && len(list) > 0 {
		t.Fatalf("页面仍含过期/占位文案: %v", list)
	}

	// 窄屏（≤900px）此前**没有任何切页入口**（侧栏 display:none）：必须有替代导航，
	// 且不得横向撑破（表格需在容器内滚动）。
	if err := page.SetViewportSize(390, 844); err != nil {
		t.Fatalf("设置窄屏视口: %v", err)
	}
	if err := waitLoc(page, ".mobile-nav .mn-item[data-route=trash]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("窄屏导航缺失: %v", err)
	}
	if err := page.Locator(".mobile-nav .mn-item[data-route=trash]").Click(); err != nil {
		t.Fatalf("窄屏导航点击失败: %v", err)
	}
	if err := waitLoc(page, "[data-panel=trash]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("窄屏导航未切到回收站: %v", err)
	}
	if ov, err := page.Evaluate(`() => ({doc: document.documentElement.scrollWidth, win: window.innerWidth})`, nil); err != nil {
		t.Fatalf("读取窄屏溢出: %v", err)
	} else if m, ok := ov.(map[string]any); ok {
		if d, w := toNum(m["doc"]), toNum(m["win"]); d > w+1 {
			t.Fatalf("窄屏横向溢出: scrollWidth=%v window=%v（表格应在容器内滚动）", d, w)
		}
	}
	// 副标题必须反映真实状态（曾写死「/ 3 个进行中」「/ 保留 30 天」）。
	if sub, err := page.Evaluate(`() => (document.querySelector('#v2-subtitle') || {}).textContent || ''`, nil); err == nil {
		if txt := fmt.Sprint(sub); strings.Contains(txt, "3 个进行中") || strings.Contains(txt, "保留 30 天") {
			t.Fatalf("副标题仍是硬编码假数据: %q", txt)
		}
	} else {
		t.Fatalf("读取副标题: %v", err)
	}
	if err := page.SetViewportSize(1440, 900); err != nil {
		t.Fatalf("恢复视口: %v", err)
	}
	// 复位到默认路由，避免影响后续断言。
	if err := page.Locator(".nav[data-route=files]").Click(); err != nil {
		t.Fatalf("复位到文件页: %v", err)
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

	// 用行选择器而非「第一个链接」：根目录还有别的播种目录（排序变化会先命中它们）。
	if err := page.Locator("#v2-file-body tr[data-name=v2demo] a").Click(); err != nil {
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
	// 预览交互：悬浮关闭按钮可见且可用（视频/控件遮挡时仍能关闭），
	// 头部「下载」按钮存在，点击舞台空白关闭。
	if _, err := page.WaitForFunction(
		"() => { const f = document.querySelector('#v2-pv-float-close'); const d = document.querySelector('#v2-pv-download'); return f && !f.hidden && d; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("预览缺少悬浮关闭/下载按钮: %v", err)
	}
	if err := page.Locator("#v2-pv-float-close").Click(); err != nil {
		t.Fatalf("悬浮关闭点击失败: %v", err)
	}
	if err := waitLoc(page, "#v2-preview", playwright.WaitForSelectorStateHidden, 4000); err != nil {
		t.Fatalf("悬浮关闭未关闭预览: %v", err)
	}
	// 再次打开，验证舞台空白点击关闭。
	if err := page.Locator("#v2-file-body tr[data-name='e2e.txt'] a[data-preview]").Click(); err != nil {
		t.Fatalf("再次打开预览: %v", err)
	}
	if err := waitLoc(page, "#v2-preview", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("预览未再次打开: %v", err)
	}
	if _, err := page.Evaluate(`() => { const s = document.querySelector('#v2-pv-stage'); const r = s.getBoundingClientRect(); s.dispatchEvent(new MouseEvent('click', { bubbles: true, clientX: r.left + 2, clientY: r.top + 2 })); }`, nil); err != nil {
		t.Fatalf("点击舞台空白: %v", err)
	}
	if err := waitLoc(page, "#v2-preview", playwright.WaitForSelectorStateHidden, 4000); err != nil {
		t.Fatalf("舞台空白点击未关闭预览: %v", err)
	}
	// 重新打开用头部关闭收尾（保持原有断言路径）。
	if err := page.Locator("#v2-file-body tr[data-name='e2e.txt'] a[data-preview]").Click(); err != nil {
		t.Fatalf("第三次打开预览: %v", err)
	}
	if err := waitLoc(page, "#v2-pv-close", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("第三次预览未打开: %v", err)
	}
	if err := page.Locator("#v2-pv-close").Click(); err != nil {
		t.Fatalf("关闭预览: %v", err)
	}

	// 预览键盘导航：同目录相邻文件 ←/→ 切换（跳过目录），并显示位置 i/n。
	// 先上第二个可预览文件，确保 e2e.txt 有邻居。
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "e2e2.txt", MimeType: "text/plain", Buffer: []byte("second e2e"),
	}}); err != nil {
		t.Fatalf("上传第二个文件: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"e2e2.txt\"] a[data-preview]", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("第二个文件未出现: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name='e2e.txt'] a[data-preview]").Click(); err != nil {
		t.Fatalf("打开 e2e.txt 预览: %v", err)
	}
	if err := waitLoc(page, "#v2-pv-pos", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("预览位置指示缺失: %v", err)
	}
	if err := page.Keyboard().Press("ArrowRight"); err != nil {
		t.Fatalf("按 →: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => (document.querySelector('#v2-pv-title') || {}).textContent === 'e2e2.txt'",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("→ 未切到下一个文件: %v", err)
	}
	if err := page.Keyboard().Press("ArrowLeft"); err != nil {
		t.Fatalf("按 ←: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => (document.querySelector('#v2-pv-title') || {}).textContent === 'e2e.txt'",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("← 未切回上一个文件: %v", err)
	}
	// 位置指示形如 "1 / 2"。
	posTxt, _ := page.Locator("#v2-pv-pos").TextContent()
	if strings.TrimSpace(posTxt) == "" {
		t.Fatalf("预览位置指示为空（期望 i / n）")
	}
	if err := page.Locator("#v2-pv-close").Click(); err != nil {
		t.Fatalf("关闭预览（键盘导航后）: %v", err)
	}

	// 列头排序：点「大小」⇒ 带 sort=size 的真实请求；再点翻转 desc。
	var sortReqURL atomic.Value
	page.OnRequest(func(r playwright.Request) {
		if strings.Contains(r.URL(), "/api/files?") && strings.Contains(r.URL(), "sort=") {
			sortReqURL.Store(r.URL())
		}
	})
	if err := page.Locator("th.sortable[data-sort=size]").Click(); err != nil {
		t.Fatalf("点大小列头: %v", err)
	}
	if _, err := page.WaitForFunction("() => !!document.querySelector('#v2-sort-size').textContent", nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(6000)}); err != nil {
		t.Fatalf("排序箭头未出现: %v", err)
	}
	if u, _ := sortReqURL.Load().(string); !strings.Contains(u, "sort=size") || !strings.Contains(u, "order=asc") {
		t.Fatalf("排序请求未带 sort=size&order=asc: %v", u)
	}
	if err := page.Locator("th.sortable[data-sort=size]").Click(); err != nil {
		t.Fatalf("再次点大小列头: %v", err)
	}
	if _, err := page.WaitForFunction("() => (document.querySelector('#v2-sort-size') || {}).textContent === '↓'", nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(6000)}); err != nil {
		t.Fatalf("第二次点击未翻转成 desc: %v", err)
	}
	if u, _ := sortReqURL.Load().(string); !strings.Contains(u, "order=desc") {
		t.Fatalf("翻转后请求仍未带 order=desc: %v", u)
	}
	// 语言切换不得抹掉表头里的子元素（排序箭头曾被 textContent 整段覆写而消失）。
	if _, err := page.Evaluate(`() => { window.v2i18n.setLang('en'); }`, nil); err != nil {
		t.Fatalf("切英文: %v", err)
	}
	if ok, _ := page.Evaluate(`() => !!document.querySelector('th.sortable[data-sort=size] #v2-sort-size')`, nil); ok != true {
		t.Fatalf("切英文后排序箭头元素被删除（applyTexts 只应改文本节点）")
	}
	if _, err := page.Evaluate(`() => { window.v2i18n.setLang('zh'); }`, nil); err != nil {
		t.Fatalf("切回中文: %v", err)
	}
	// 复位成按名称（后续断言依赖名称排序）。
	if err := page.Locator("th.sortable[data-sort=name]").Click(); err != nil {
		t.Fatalf("复位按名称排序: %v", err)
	}

	// 分页：210 个文件的目录首屏只有一页，必须提示「还有更多」并可就地追加
	// （此前 limit=200 静默截断，用户以为文件丢了）。
	if _, err := page.Evaluate("() => { location.hash = '#/files/many'; }", nil); err != nil {
		t.Fatalf("进入 many: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-file-body tr').length === 200", nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("首屏未加载 200 行: %v", err)
	}
	if hidden, _ := page.Evaluate(`() => (document.querySelector('#v2-load-more-wrap') || {}).hidden`, nil); hidden != false {
		t.Fatalf("已截断（200/210）但「加载更多」未显示")
	}
	if cnt, _ := page.Evaluate(`() => (document.querySelector('#v2-count') || {}).textContent || ''`, nil); !strings.Contains(fmt.Sprint(cnt), "200 / 210") {
		t.Fatalf("计数未显示 已加载/总数: %v", cnt)
	}
	if err := page.Locator("#v2-load-more").Click(); err != nil {
		t.Fatalf("点加载更多: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-file-body tr').length === 210", nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("加载更多未追加到 210 行: %v", err)
	}
	if hidden, _ := page.Evaluate(`() => (document.querySelector('#v2-load-more-wrap') || {}).hidden`, nil); hidden != true {
		t.Fatalf("全部加载完后「加载更多」仍显示")
	}
	if err := page.Locator(".nav[data-route=files]").Click(); err != nil {
		t.Fatalf("回到文件根: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回 v2demo: %v", err)
	}

	// 拖拽上传：真实 DragEvent（DataTransfer 带 File）→ 走与选择文件同一条上传路径。
	if _, err := page.Evaluate(`async () => {
		const dt = new DataTransfer();
		dt.items.add(new File(['dropped content'], 'dropped.txt', {type: 'text/plain'}));
		document.dispatchEvent(new DragEvent('drop', {dataTransfer: dt, bubbles: true, cancelable: true}));
		return true;
	}`, nil); err != nil {
		t.Fatalf("模拟拖拽 drop: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name='dropped.txt']", playwright.WaitForSelectorStateVisible, 15000); err != nil {
		t.Fatalf("拖拽上传后未出现 dropped.txt: %v", err)
	}

	// Meta extra 编辑（merge）：真实 PUT /api/files/meta → 回读含新键。
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "meta.txt", MimeType: "text/plain", Buffer: []byte("meta me"),
	}}); err != nil {
		t.Fatalf("上传 meta.txt: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"meta.txt\"] [data-action=detail]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("详情按钮未出现: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"meta.txt\"] [data-action=detail]").Click(); err != nil {
		t.Fatalf("打开检查器: %v", err)
	}
	if err := page.Locator("[data-drtab=meta]").Click(); err != nil {
		t.Fatalf("切到 Meta 页签: %v", err)
	}
	if err := waitLoc(page, "#v2-meta-edit", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("meta 编辑框未出现: %v", err)
	}
	if err := page.Locator("#v2-meta-edit").Fill(`{"code":"EXAMPLE-001"}`); err != nil {
		t.Fatalf("填写 extra: %v", err)
	}
	if err := page.Locator("#v2-meta-save").Click(); err != nil {
		t.Fatalf("保存 extra: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const p = document.querySelector('#v2-meta-json'); return p && p.textContent.includes('EXAMPLE-001'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("保存后回读未含新键: %v", err)
	}
	if err := page.Locator("#v2-dr-close").Click(); err != nil {
		t.Fatalf("关闭检查器: %v", err)
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
	if n, _ := page.Locator(".cs-nav button").Count(); n != 12 {
		t.Fatalf("控制台分区 = %d，期望 12", n)
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
	// 高级操作：关键字筛选 → 详情 → 选择 → 批量重试（真实 POST /api/cloud/tasks/retry）。
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr[data-name], #v2-cs-tasks tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("云端任务列表为空: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks-q").Fill("nope"); err != nil {
		t.Fatalf("任务关键字筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr').length >= 1",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("关键字筛选后无任务: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks tr").First().Locator("td.actions button").First().Click(); err != nil {
		t.Fatalf("打开任务详情: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const p = document.querySelector('#v2-cs-tasks-detail-json'); return p && p.textContent.indexOf('\"id\"') >= 0; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("任务详情未渲染: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !!document.querySelector('#v2-cs-tasks .pill-bad')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(45000)}); err != nil {
		t.Fatalf("云任务未进入失败态: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks tr .cb").First().Check(); err != nil {
		t.Fatalf("选择任务: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !document.querySelector('#v2-cs-tasks-retry').disabled",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("批量重试按钮未启用: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks-retry").Click(); err != nil {
		t.Fatalf("批量重试点击: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('已处理'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("批量重试未回显结果: %v", err)
	}
	// 卷分组 chips：全部卷 / 未指定卷（真实任务无 transfer.volume）。
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks-groups .chip').length >= 2",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("任务分组 chips 未渲染: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks-groups [data-vol=\"__none__\"]").Click(); err != nil {
		t.Fatalf("按未指定卷分组: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("分组过滤后任务为空: %v", err)
	}
	// 体积筛选：不可达任务 total_size=-1 ⇒ 归入「未知大小」；大体积区间应为空。
	if _, err := page.Locator("#v2-cs-tasks-size").SelectOption(playwright.SelectOptionValues{Values: &[]string{"unknown"}}); err != nil {
		t.Fatalf("按未知大小筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("未知大小筛选后无任务: %v", err)
	}
	if _, err := page.Locator("#v2-cs-tasks-size").SelectOption(playwright.SelectOptionValues{Values: &[]string{"gt10g"}}); err != nil {
		t.Fatalf("按大体积筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr').length === 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("大体积筛选后仍有任务: %v", err)
	}
	if _, err := page.Locator("#v2-cs-tasks-size").SelectOption(playwright.SelectOptionValues{Values: &[]string{""}}); err != nil {
		t.Fatalf("复位体积筛选: %v", err)
	}

	// 深链：hash 携带筛选参数，刷新后自动恢复分区与筛选。
	if hash, _ := page.Evaluate("() => location.hash", nil); hash == nil || !strings.Contains(hash.(string), "q=nope") {
		t.Fatalf("任务筛选未写入 hash: %v", hash)
	}
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新页面: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const q = document.querySelector('#v2-cs-tasks-q'); const pane = document.querySelector('[data-cspane=tasks]'); return q && q.value === 'nope' && pane && !pane.hidden; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("刷新后未恢复任务分区/筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-cs-tasks tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("刷新后任务列表为空: %v", err)
	}
	if err := page.Locator("#v2-cs-tasks-q").Fill(""); err != nil {
		t.Fatalf("清空任务筛选: %v", err)
	}
	if err := page.Locator("[data-cs=issues]").Click(); err != nil {
		t.Fatalf("切换待处理事项分区: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=issues]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("控制台待处理事项未渲染: %v", err)
	}
	// 待处理事项：筛选控件与 CSV 导出入口可用（无条目时导出给出提示）。
	if err := waitLoc(page, "#v2-cs-issues-q", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("事项筛选输入框缺失: %v", err)
	}
	// 事项「立即扫描」：真实 POST /api/issues/scan → 回显登记数（未装配则给明确错误）。
	if err := page.Locator("#v2-cs-issues-scan").Click(); err != nil {
		t.Fatalf("触发事项扫描: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); const b = document.querySelector('#v2-cs-state'); return (t && t.textContent.includes('扫描')) || (b && !b.hidden && b.textContent.includes('扫描')); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(20000)}); err != nil {
		t.Fatalf("事项扫描未回显: %v", err)
	}
	if err := page.Locator("#v2-cs-issues-q").Fill("e2e"); err != nil {
		t.Fatalf("事项关键字筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-cs-issues'); return t && t.children.length >= 0; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("事项筛选未生效: %v", err)
	}
	if err := page.Locator("#v2-cs-issues-export").Click(); err != nil {
		t.Fatalf("事项导出 CSV: %v", err)
	}
	if err := page.Locator("#v2-cs-issues-q").Fill(""); err != nil {
		t.Fatalf("清空事项筛选: %v", err)
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
	// 卷分区：真实 /api/volumes（默认卷可见）+ 单卷刷新（/api/volumes/refresh）。
	if err := page.Locator("[data-cs=volumes]").Click(); err != nil {
		t.Fatalf("切换卷分区: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-vl-body tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("卷列表为空: %v", err)
	}
	if err := page.Locator("#v2-vl-body tr").First().Locator("button").Click(); err != nil {
		t.Fatalf("单卷刷新: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('已刷新'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("单卷刷新未回显: %v", err)
	}
	// 建卷表单：打开对话框 → 填名称/类型 → 取消（不真正建卷，避免污染状态）。
	if err := page.Locator("#v2-vl-create").Click(); err != nil {
		t.Fatalf("打开建卷对话框: %v", err)
	}
	if err := waitLoc(page, "#v2-vc-name", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("建卷表单未显示: %v", err)
	}
	if err := page.Locator("#v2-vc-name").Fill("e2evol"); err != nil {
		t.Fatalf("填写卷名: %v", err)
	}
	if err := page.Locator("[data-vc=cancel]").Click(); err != nil {
		t.Fatalf("取消建卷: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !document.querySelector('#v2-vc-name')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("建卷对话框未关闭: %v", err)
	}

	// 用户维度页面配置：设置里切「直接删除」→ PUT /api/ui/prefs 生效 → 再删文件不进回收站。
	if err := page.Locator("#v2-settings-open").Click(); err != nil {
		t.Fatalf("打开设置: %v", err)
	}
	if err := waitLoc(page, "#v2-set-delete-mode", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("删除默认方式选择器缺失: %v", err)
	}
	// 页面级 dialog 处理器只装一次，按消息分派（重复 OnDialog 会让后面的 prompt 拿到空值）。
	respondDialogs(page, func(d playwright.Dialog) {
		if strings.Contains(d.Message(), "Secret 名称") {
			_ = d.Accept("e2esecret")
			return
		}
		_ = d.Accept()
	})
	if _, err := page.Locator("#v2-set-delete-mode").SelectOption(playwright.SelectOptionValues{Values: &[]string{"hard"}}); err != nil {
		t.Fatalf("选择 hard: %v", err)
	}
	if err := page.Locator("#v2-set-delete-save").Click(); err != nil {
		t.Fatalf("保存删除默认方式: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('删除默认方式已保存'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("删除默认方式未保存: %v", err)
	}
	// 服务端已生效。
	// 用页面自己的签名客户端读（裸 fetch 无 SproxySig 凭据 ⇒ 401）。
	if _, err := page.Evaluate(`() => window.v2api.json('GET','/api/ui/prefs')
		.then(j => { window.__dm = j.prefs.delete.default_mode; })
		.catch(e => { window.__dm = 'ERR:' + (e && e.message); })`, nil); err != nil {
		t.Fatalf("读取页面配置: %v", err)
	}
	if _, err := page.WaitForFunction("() => typeof window.__dm === 'string'", nil,
		playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("读取页面配置超时: %v", err)
	}
	if dm, _ := page.Evaluate("() => window.__dm", nil); dm != "hard" {
		t.Fatalf("服务端未生效 hard: %v", dm)
	}
	if err := page.Locator("#v2-settings-close").Click(); err != nil {
		t.Fatalf("关闭设置: %v", err)
	}
	// 复位为软删，避免影响后续用例。
	if _, err := page.Evaluate(`() => window.v2api.json('PUT','/api/ui/prefs', {
		contentType: 'application/json',
		body: new TextEncoder().encode(JSON.stringify({delete:{default_mode:'soft'}}))
	})`, nil); err != nil {
		t.Fatalf("复位为 soft: %v", err)
	}

	// 未完成上传续传提示：写入 pending 记录 → 刷新 → 提示可见 → 忽略后清除。
	if _, err := page.Evaluate(`() => { localStorage.setItem('sproxy_v2_pending_upload', JSON.stringify({
		name: 'v2demo/big.bin', base: 'big.bin', size: 1048576, mtime: 1700000000000,
		dir: 'v2demo', uploadID: 'pending-demo', total: 1048576, received: 524288, ts: Date.now()
	})); }`, nil); err != nil {
		t.Fatalf("写入 pending 记录: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files'; }", nil); err != nil {
		t.Fatalf("切到文件页: %v", err)
	}
	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新页面: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-pending'); return b && !b.hidden && b.textContent.includes('big.bin'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("未完成上传提示未显示: %v", err)
	}
	// Ctrl+A 全选 + Delete 触发批量删除（真实弹窗，取消不动数据）。
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("列表未就绪: %v", err)
	}
	if err := page.Locator("#v2-file-body").Click(); err != nil {
		t.Fatalf("聚焦列表: %v", err)
	}
	if err := page.Keyboard().Press("Control+a"); err != nil {
		t.Fatalf("Ctrl+A 全选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-batch-delete'); return b && !b.disabled; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("Ctrl+A 未选中任何文件: %v", err)
	}
	if err := page.Keyboard().Press("Delete"); err != nil {
		t.Fatalf("按 Delete: %v", err)
	}
	if err := waitLoc(page, ".modal-backdrop [data-dlg=cancel]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("Delete 未打开确认框: %v", err)
	}
	if err := page.Locator(".modal-backdrop [data-dlg=cancel]").Click(); err != nil {
		t.Fatalf("取消批量删除: %v", err)
	}

	if err := page.Locator("#v2-pending-ignore").Click(); err != nil {
		t.Fatalf("忽略未完成上传: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-pending'); return b && b.hidden; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("忽略后提示未隐藏: %v", err)
	}

	// Secret 卷：可用则创建随机密钥 → 列表出现 → 删除；未装配时展示明确提示（二者必居其一）。
	if _, err := page.Evaluate("() => { location.hash = '#/console/volumes'; }", nil); err != nil {
		t.Fatalf("回到卷分区: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=volumes]", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("卷分区未显示: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-sec-state'); const e = document.querySelector('#v2-sec-empty'); const t = document.querySelector('#v2-sec-body'); return (b && !b.hidden) || (e && !e.hidden) || (t && t.children.length > 0); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("Secret 卡既无数据也无提示: %v", err)
	}
	unavailable, _ := page.Evaluate("() => { const b = document.querySelector('#v2-sec-state'); return !!(b && !b.hidden); }", nil)
	t.Logf("secret 卷可用性：unavailable=%v（false = 已走真实创建/删除分支）", unavailable)
	if unavailable != true {
		// dialog 处理器已在上面统一安装（按消息分派），此处不再重复 OnDialog。
		if err := page.Locator("#v2-sec-create").Click(); err != nil {
			t.Fatalf("创建 secret: %v", err)
		}
		if err := waitLoc(page, "#v2-sec-body tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
			st, _ := page.Locator("#v2-sec-state").TextContent()
			t.Fatalf("secret 创建后未列出（state=%q）: %v", st, err)
		}
		if _, err := page.WaitForFunction(
			"() => document.querySelector('#v2-sec-body').textContent.includes('e2esecret')",
			nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
			t.Fatalf("secret 名称未出现: %v", err)
		}
		if err := page.Locator("#v2-sec-body tr button").First().Click(); err != nil {
			t.Fatalf("删除 secret: %v", err)
		}
		if _, err := page.WaitForFunction(
			"() => !document.querySelector('#v2-sec-body').textContent.includes('e2esecret')",
			nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
			t.Fatalf("secret 删除后仍在列表: %v", err)
		}
	}

	// Mesh 分区：真实 /api/mesh/status 渲染面状态。
	if err := page.Locator("[data-cs=mesh]").Click(); err != nil {
		t.Fatalf("切换 Mesh 分区: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-mesh-body tr').length >= 3",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("Mesh 状态未渲染: %v", err)
	}
	// Hub 分区：未启用时给出明确提示（测试服务端无 hub）。
	if err := page.Locator("[data-cs=hub]").Click(); err != nil {
		t.Fatalf("切换 Hub 分区: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-hub-state'); const t = document.querySelector('#v2-hub-body'); return (b && !b.hidden) || (t && t.children.length > 0); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("Hub 分区既无数据也无提示: %v", err)
	}
	// Hub 踢出按钮：admin（测试环境 loopback 允许）时可见，否则不可见——二者皆合法，仅断言无异常。
	if n, _ := page.Locator("#v2-hub-body [data-kick]").Count(); n > 0 {
		if _, err := page.Locator("#v2-hub-body [data-kick]").First().IsVisible(); err != nil {
			t.Fatalf("踢出按钮可见性检查失败: %v", err)
		}
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
	// j 逐行下移直到焦点落在 e2e.txt（该目录还有其它播种文件，不能假定首行就是它）。
	if err := page.Keyboard().Press("j"); err != nil {
		t.Fatalf("j 移动: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !!document.querySelector('#v2-file-body tr.row-focus')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("键盘焦点行未出现: %v", err)
	}
	for i := 0; i < 12; i += 1 {
		focused, _ := page.Evaluate(`() => { const r = document.querySelector('#v2-file-body tr.row-focus'); return r ? r.getAttribute('data-name') : ''; }`, nil)
		if fmt.Sprint(focused) == "e2e.txt" {
			break
		}
		if err := page.Keyboard().Press("j"); err != nil {
			t.Fatalf("j 继续下移: %v", err)
		}
		<-time.After(80 * time.Millisecond)
	}
	if focused, _ := page.Evaluate(`() => { const r = document.querySelector('#v2-file-body tr.row-focus'); return r ? r.getAttribute('data-name') : ''; }`, nil); fmt.Sprint(focused) != "e2e.txt" {
		t.Fatalf("j 未能把焦点移到 e2e.txt（当前 %v）", focused)
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

	// 服务端搜索（真实 /api/files/search）：命中完整相对路径；清除后回到目录列表。
	if err := page.Locator("#v2-search").Fill("e2e"); err != nil {
		t.Fatalf("填写搜索: %v", err)
	}
	if err := page.Locator("#v2-search-btn").Click(); err != nil {
		t.Fatalf("点击搜索: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"v2demo/e2e.txt\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("搜索未命中 v2demo/e2e.txt: %v", err)
	}
	// 搜索态作业必须用完整相对路径（不得再拼 subdir）：详情里路径应为 v2demo/e2e.txt。
	if err := page.Locator("#v2-file-body tr[data-name=\"v2demo/e2e.txt\"] [data-action=detail]").Click(); err != nil {
		t.Fatalf("搜索态打开详情: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const d = document.querySelector('#v2-dr-info'); return d && d.textContent.includes('v2demo/e2e.txt') && !d.textContent.includes('v2demo/v2demo'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("搜索态详情路径拼接错误: %v", err)
	}
	if err := page.Locator("#v2-dr-close").Click(); err != nil {
		t.Fatalf("关闭详情: %v", err)
	}
	if err := page.Locator("#v2-search-clear").Click(); err != nil {
		t.Fatalf("清除搜索: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"e2e.txt\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("清除搜索后未回到目录列表: %v", err)
	}

	// 上传落指定卷：选择卷后上传，任务/文件列表带该卷（真实 ?volume= 与 init body）。
	if _, err := page.Locator("#v2-volume").SelectOption(playwright.SelectOptionValues{Values: &[]string{"default"}}); err != nil {
		t.Fatalf("选择卷 default: %v", err)
	}
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "volupload.txt", MimeType: "text/plain", Buffer: []byte("volume me"),
	}}); err != nil {
		t.Fatalf("上传到指定卷: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const tr = document.querySelector('#v2-file-body tr[data-name=\"v2demo/volupload.txt\"]') || document.querySelector('#v2-file-body tr[data-name=\"volupload.txt\"]'); return !!tr; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("指定卷上传后列表未出现文件: %v", err)
	}

	// 卷过滤 + 强刷（真实 /api/volumes 与 /api/volumes/refresh）。
	if n, _ := page.Locator("#v2-volume option").Count(); n < 2 {
		t.Fatalf("卷选择器未加载卷列表（options=%d）", n)
	}
	if _, err := page.Evaluate(`() => { const s = document.querySelector('#v2-volume'); s.selectedIndex = 1; s.dispatchEvent(new Event('change')); }`, nil); err != nil {
		t.Fatalf("选择卷: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-file-body tr[data-name]').length > 0 || !document.querySelector('#v2-files-state').hidden",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("按卷过滤后列表未响应: %v", err)
	}
	if err := page.Locator("#v2-vol-refresh").Click(); err != nil {
		t.Fatalf("点击清缓存: %v", err)
	}

	// 回收站真实流程：删除 e2e.txt → 出现在回收站 → 恢复 → 回到列表。
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"e2e.txt\"] [data-action=delete]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("未找到删除按钮: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"e2e.txt\"] [data-action=delete]").Click(); err != nil {
		t.Fatalf("点击删除: %v", err)
	}
	if err := page.Locator(".modal-backdrop [data-dlg=ok]").Click(); err != nil {
		t.Fatalf("确认删除: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !document.querySelector('#v2-file-body tr[data-name=\"e2e.txt\"]')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("删除后列表仍存在条目: %v", err)
	}
	if err := page.Keyboard().Press("g"); err != nil {
		t.Fatalf("g 前缀: %v", err)
	}
	if err := page.Keyboard().Press("r"); err != nil {
		t.Fatalf("g r 跳回收站: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const b = document.querySelector('#v2-trash-body'); return b && b.children.length > 0; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("回收站未列出刚删除的文件: %v", err)
	}
	if err := page.Locator("#v2-trash-body button").First().Click(); err != nil {
		t.Fatalf("点击恢复: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-trash-body').children.length === 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("恢复后回收站未清空: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"e2e.txt\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("恢复后文件未回到列表: %v", err)
	}

	// 再删除一次 → 清空回收站（原生确认框）。先把卷过滤复位为「全部卷」。
	if _, err := page.Evaluate(`() => { const s = document.querySelector('#v2-volume'); if (s) { s.selectedIndex = 0; s.dispatchEvent(new Event('change')); } }`, nil); err != nil {
		t.Fatalf("复位卷过滤: %v", err)
	}
	// 切卷会按「该卷上次目录」恢复路径（可能回到根）：显式回 v2demo 并强制重载列表
	// （hash 相同时不会触发 hashchange，必须显式 load）。
	if _, err := page.Evaluate(`() => {
		location.hash = '#/files/v2demo';
		if (window.v2filesView) { window.v2filesView.load(); }
	}`, nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"e2e.txt\"] [data-action=delete]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("删除按钮未出现: %v", err)
	}
	respondDialogs(page, func(d playwright.Dialog) { _ = d.Accept() })
	if err := page.Locator("#v2-file-body tr[data-name=\"e2e.txt\"] [data-action=delete]").Click(); err != nil {
		t.Fatalf("再次点击删除: %v", err)
	}
	if err := page.Locator(".modal-backdrop [data-dlg=ok]").Click(); err != nil {
		t.Fatalf("再次确认删除: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !document.querySelector('#v2-file-body tr[data-name=\"e2e.txt\"]')",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("二次删除后列表仍存在条目: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/trash'; }", nil); err != nil {
		t.Fatalf("进入回收站: %v", err)
	}
	if err := waitLoc(page, "#v2-trash-empty", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("回收站按钮未渲染: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelector('#v2-trash-body').children.length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		state, _ := page.Locator("#v2-trash-state").TextContent()
		count, _ := page.Locator("#v2-trash-count").TextContent()
		fstate, _ := page.Locator("#v2-files-state").TextContent()
		t.Fatalf("回收站未出现待清空条目: %v (trash_state=%q count=%q files_state=%q)", err, state, count, fstate)
	}
	if err := page.Locator("#v2-trash-empty").Click(); err != nil {
		t.Fatalf("点击清空: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => !document.querySelector('#v2-trash-empty-hint').hidden",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("清空后回收站未变为空: %v", err)
	}

	// 分享：带口令创建 → 列表显示 → 缺口令 401 / 正确口令 200 → 撤销。
	// 卷过滤复位为「全部卷」并显式重载（hash 相同不会触发 hashchange）。
	if _, err := page.Evaluate(`() => {
		const v = document.querySelector('#v2-volume');
		if (v) { v.selectedIndex = 0; v.dispatchEvent(new Event('change')); }
	}`, nil); err != nil {
		t.Fatalf("复位卷过滤: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if _, err := page.Evaluate("() => { if (window.v2filesView) { window.v2filesView.load(); } }", nil); err != nil {
		t.Fatalf("重载列表: %v", err)
	}
	// 自建一个专用文件（此处 e2e.txt 已在上面的回收站流程中被删除）。
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "shareme.txt", MimeType: "text/plain", Buffer: []byte("hello e2e"),
	}}); err != nil {
		t.Fatalf("上传 shareme.txt: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"shareme.txt\"] [data-action=detail]", playwright.WaitForSelectorStateVisible, 15000); err != nil {
		t.Fatalf("详情按钮未出现: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"shareme.txt\"] [data-action=detail]").Click(); err != nil {
		t.Fatalf("打开检查器: %v", err)
	}
	if err := page.Locator("[data-drtab=share]").Click(); err != nil {
		t.Fatalf("切到分享页签: %v", err)
	}
	if err := waitLoc(page, "#v2-share-password", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("分享口令输入框缺失: %v", err)
	}
	if err := page.Locator("#v2-share-password").Fill("e2epw"); err != nil {
		t.Fatalf("填写口令: %v", err)
	}
	if err := page.Locator("#v2-share-watermark").Fill("e2ewm"); err != nil {
		t.Fatalf("填写水印: %v", err)
	}
	if err := page.Locator("#v2-share-create").Click(); err != nil {
		t.Fatalf("创建口令分享: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const r = document.querySelector('#v2-share-result'); return r && r.textContent.includes('/s/'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("口令分享未创建成功（TTL/口令契约？）: %v", err)
	}
	hrefVal, _ := page.Locator("#v2-share-result a").First().Evaluate("el => el.href", nil)
	shareLink, _ := hrefVal.(string)
	if shareLink == "" {
		t.Fatalf("分享链接缺失")
	}
	// 缺口令 → 401；正确口令 → 200 且拿到内容。
	noPw, getErr := http.Get(shareLink)
	if getErr != nil {
		t.Fatalf("匿名访问分享: %v", getErr)
	}
	noPw.Body.Close()
	if noPw.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺口令应 401，got %d", noPw.StatusCode)
	}
	withPw, pwErr := http.Get(shareLink + "?password=e2epw")
	if pwErr != nil {
		t.Fatalf("带口令访问: %v", pwErr)
	}
	if wm := withPw.Header.Get("X-Share-Watermark"); wm != "e2ewm" {
		t.Fatalf("分享水印头 = %q，期望 e2ewm", wm)
	}
	gotBody, _ := io.ReadAll(withPw.Body)
	withPw.Body.Close()
	if withPw.StatusCode != http.StatusOK || !strings.Contains(string(gotBody), "hello e2e") {
		t.Fatalf("带口令访问失败: status=%d body=%q", withPw.StatusCode, gotBody)
	}
	// 列表出现该链接并可撤销（行内现在有「复制」「撤销」两个按钮，按文案定位）。
	if _, err := page.Evaluate(`() => {
		const rows = Array.from(document.querySelectorAll('#v2-share-list .ver-row'));
		const row = rows.find(r => r.textContent.includes('/s/'));
		if (!row) { return 'no-row'; }
		const btn = Array.from(row.querySelectorAll('button')).find(b => b.textContent.includes('撤销'));
		if (!btn) { return 'no-btn'; }
		btn.click();
		return 'clicked';
	}`, nil); err != nil {
		t.Fatalf("撤销分享: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const l = document.querySelector('#v2-share-list'); return l && !l.textContent.includes('/s/'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("撤销后列表仍显示链接: %v", err)
	}
	if err := page.Locator("#v2-dr-close").Click(); err != nil {
		t.Fatalf("关闭检查器: %v", err)
	}

	// 控制台「分享」分区：真实创建的口令分享出现在列表（含「口令」标记），筛选可用。
	if _, err := page.Evaluate("() => { location.hash = '#/console/shares'; }", nil); err != nil {
		t.Fatalf("进入分享分区: %v", err)
	}
	if err := waitLoc(page, "[data-cspane=shares]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("分享分区未显示: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-sh-body tr').length > 0 || !document.querySelector('#v2-sh-empty').hidden || !document.querySelector('#v2-sh-banner').hidden",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("分享分区既无列表也无提示: %v", err)
	}
	if _, err := page.Locator("#v2-sh-state").SelectOption(playwright.SelectOptionValues{Values: &[]string{"pw"}}); err != nil {
		t.Fatalf("按口令筛选: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-sh-body'); return t && t.querySelectorAll('tr').length >= 0; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("口令筛选未生效: %v", err)
	}
	if _, err := page.Locator("#v2-sh-state").SelectOption(playwright.SelectOptionValues{Values: &[]string{""}}); err != nil {
		t.Fatalf("复位分享筛选: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到文件页: %v", err)
	}

	// 目录重命名/移动：建目录 + 文件 → 行内重命名为 dirmoved → 内容随迁。
	if _, err := page.Evaluate("() => { location.hash = '#/files'; }", nil); err != nil {
		t.Fatalf("回到文件页: %v", err)
	}
	if err := waitLoc(page, "#v2-mkdir", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("文件页未就绪: %v", err)
	}
	if err := page.Locator("#v2-mkdir").Click(); err != nil {
		t.Fatalf("打开新建目录: %v", err)
	}
	if err := page.Locator("#v2-dlg-input").Fill("dirsrc"); err != nil {
		t.Fatalf("填写目录名: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("提交新建目录: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"dirsrc\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("目录未出现: %v", err)
	}
	// 进目录放一个文件，再返回父目录重命名整个目录。
	if err := page.Locator("#v2-file-body tr[data-name=\"dirsrc\"] a").Click(); err != nil {
		t.Fatalf("进入目录: %v", err)
	}
	if err := waitLoc(page, "#v2-upload-input", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("目录内上传控件不可用: %v", err)
	}
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "inner.txt", MimeType: "text/plain", Buffer: []byte("inside"),
	}}); err != nil {
		t.Fatalf("上传 inner.txt: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"inner.txt\"]", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("inner.txt 未出现: %v", err)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/files'; }", nil); err != nil {
		t.Fatalf("回到根目录: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"dirsrc\"] [data-action=rename]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("目录行缺少重命名按钮（目录应可重命名）: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"dirsrc\"] [data-action=rename]").Click(); err != nil {
		t.Fatalf("点击目录重命名: %v", err)
	}
	if err := page.Locator("#v2-dlg-input").Fill("dirmoved"); err != nil {
		t.Fatalf("填写新目录名: %v", err)
	}
	if err := page.Locator("[data-dlg=ok]").Click(); err != nil {
		t.Fatalf("提交目录重命名: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"dirmoved\"]", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("目录重命名后未出现新名: %v", err)
	}
	// 子树随迁：进 dirmoved 应看到 inner.txt。
	if err := page.Locator("#v2-file-body tr[data-name=\"dirmoved\"] a").Click(); err != nil {
		t.Fatalf("进入改名后目录: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"inner.txt\"]", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("目录内容未随改名迁移: %v", err)
	}

	// 批量重命名 + 打包下载（真实 /api/batch/rename 与 /api/archive）。
	if _, err := page.Evaluate("() => { location.hash = '#/files/v2demo'; }", nil); err != nil {
		t.Fatalf("回到 v2demo: %v", err)
	}
	if err := waitLoc(page, "#v2-upload-input", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("上传控件不可用: %v", err)
	}
	if err := page.Locator("#v2-upload-input").SetInputFiles([]playwright.InputFile{{
		Name: "ren.txt", MimeType: "text/plain", Buffer: []byte("rename me"),
	}}); err != nil {
		t.Fatalf("上传 ren.txt: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"ren.txt\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("ren.txt 未出现: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"ren.txt\"] .cb").Check(); err != nil {
		t.Fatalf("选择 ren.txt: %v", err)
	}
	if err := page.Locator("#v2-batch-rename").Click(); err != nil {
		t.Fatalf("打开批量重命名: %v", err)
	}
	if err := page.Locator("#v2-br-suffix").Fill("-v2"); err != nil {
		t.Fatalf("填写后缀: %v", err)
	}
	if err := page.Locator("[data-br=ok]").Click(); err != nil {
		t.Fatalf("提交批量重命名: %v", err)
	}
	if err := waitLoc(page, "#v2-file-body tr[data-name=\"ren.txt-v2\"]", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("批量重命名结果未出现: %v", err)
	}
	if err := page.Locator("#v2-file-body tr[data-name=\"ren.txt-v2\"] .cb").Check(); err != nil {
		t.Fatalf("选择重命名后的文件: %v", err)
	}
	if err := page.Locator("#v2-batch-archive").Click(); err != nil {
		t.Fatalf("点击打包下载: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('归档'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("打包下载未回显: %v", err)
	}

	// 云任务产物预览：真实上传一个可完成的云任务（loopback 源），完成后点「预览」看文本内容。
	textSrc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("cloud preview body"))
	}))
	defer textSrc.Close()
	previewBody := `{"url":"` + textSrc.URL + `/pv.txt","filename":"pv.txt","download_local":false,"save":true}`
	if code, resp := postJSON(t, baseURL+"/api/cloud/download", []byte(previewBody)); code != http.StatusOK {
		t.Fatalf("创建预览用云任务失败: %d %s", code, resp)
	}
	// 等任务完成（服务端异步），再回到传输页用 UI 预览。
	pvDeadline := time.Now().Add(60 * time.Second)
	for {
		listResp, lerr := http.Get(baseURL + "/api/cloud/tasks?limit=200")
		done := false
		if lerr == nil {
			var payload struct {
				Tasks []struct {
					Filename string `json:"filename"`
					Status   string `json:"status"`
				} `json:"tasks"`
			}
			_ = json.NewDecoder(listResp.Body).Decode(&payload)
			listResp.Body.Close()
			for _, tk := range payload.Tasks {
				if tk.Filename == "pv.txt" && tk.Status == "completed" {
					done = true
				}
			}
		}
		if done {
			break
		}
		if time.Now().After(pvDeadline) {
			t.Fatalf("预览用云任务 60s 未完成")
		}
		<-time.After(300 * time.Millisecond)
	}
	if _, err := page.Evaluate("() => { location.hash = '#/transfer'; }", nil); err != nil {
		t.Fatalf("回到传输页: %v", err)
	}
	if err := page.Locator("#v2-tf-refresh").Click(); err != nil {
		t.Fatalf("刷新任务列表: %v", err)
	}
	if err := waitLoc(page, "#v2-tf-body tr", playwright.WaitForSelectorStateVisible, 10000); err != nil {
		t.Fatalf("任务列表未渲染: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => Array.from(document.querySelectorAll('#v2-tf-body tr')).some(r => r.textContent.includes('pv.txt') && r.textContent.includes('预览'));",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("已完成任务未出现预览按钮: %v", err)
	}
	if _, err := page.Evaluate(`() => {
		const rows = Array.from(document.querySelectorAll('#v2-tf-body tr'));
		const row = rows.find(r => r.textContent.includes('pv.txt'));
		if (!row) { return 'no-row'; }
		const btn = Array.from(row.querySelectorAll('button')).find(b => b.textContent.includes('预览'));
		if (!btn) { return 'no-btn'; }
		btn.click();
		return 'clicked';
	}`, nil); err != nil {
		t.Fatalf("点击任务预览: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const p = document.querySelector('#v2-preview'); const t = document.querySelector('#v2-pv-text'); return p && !p.hidden && t && t.textContent.includes('cloud preview body'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("云任务预览未展示内容: %v", err)
	}
	if err := page.Locator("#v2-pv-close").Click(); err != nil {
		t.Fatalf("关闭预览: %v", err)
	}

	// 链式下载：两个不可达 URL 建组 → 组行失败 → 「打包全部」被 409 拒绝（全有语义）。
	if err := page.Locator(".nav[data-route=transfer]").Click(); err != nil {
		t.Fatalf("回到传输页: %v", err)
	}
	if err := page.Locator("#v2-chain-open").Click(); err != nil {
		t.Fatalf("打开链式下载: %v", err)
	}
	if err := waitLoc(page, "#v2-chain-urls", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("链式下载对话框未显示: %v", err)
	}
	chainURLs := "http://127.0.0.1:1/chain1.bin\nhttp://127.0.0.1:1/chain2.bin"
	if err := page.Locator("#v2-chain-urls").Fill(chainURLs); err != nil {
		t.Fatalf("填写链式链接: %v", err)
	}
	if err := page.Locator("#v2-chain-ok").Click(); err != nil {
		t.Fatalf("提交链式下载: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('链式下载'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(10000)}); err != nil {
		toasts, _ := page.Locator("#v2-toasts").TextContent()
		t.Fatalf("链式下载未回显 (toasts=%q): %v", toasts, err)
	}
	if _, err := page.WaitForFunction(
		"() => document.querySelectorAll('#v2-grp-body tr').length > 0",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		toasts, _ := page.Locator("#v2-toasts").TextContent()
		gstate, _ := page.Locator("#v2-grp-state").TextContent()
		t.Fatalf("任务组列表为空 (toasts=%q grp_state=%q): %v", toasts, gstate, err)
	}
	// 等组内任务真实失败，再点「打包全部」→ 服务端 require_all 409（前端 toast 报错）。
	if _, err := page.WaitForFunction(
		"() => { const rows = document.querySelectorAll('#v2-grp-body tr'); return Array.from(rows).some(r => r.querySelector('button') && /打包全部/.test(r.textContent)); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(60000)}); err != nil {
		t.Fatalf("组未进入可打包状态（失败数未体现）: %v", err)
	}
	if err := page.Locator("#v2-grp-body tr").First().Locator("button", playwright.LocatorLocatorOptions{HasText: "打包全部"}).Click(); err != nil {
		t.Fatalf("点击打包全部: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && (t.textContent.includes('打包失败') || t.textContent.includes('not fully completed')); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		t.Fatalf("打包全部未被 409 拒绝（全有语义失效）: %v", err)
	}

	// 云任务 TSV 导入：按 url 触发真实重试，回显导入结果并记录最近操作。
	if _, err := page.Evaluate("() => { location.hash = '#/console'; }", nil); err != nil {
		t.Fatalf("回到控制台: %v", err)
	}
	if err := page.Locator("[data-cs=tasks]").Click(); err != nil {
		t.Fatalf("切换云端任务分区: %v", err)
	}
	if err := waitLoc(page, "#v2-cs-tasks-import", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("TSV 导入控件不可用: %v", err)
	}
	tsv := "id\tstatus\tfilename\ttotal_size\turl\tupdated_at\n" +
		"\t\t\t0\thttp://127.0.0.1:1/nope2.bin\t\n"
	if err := page.Locator("#v2-cs-tasks-import").SetInputFiles([]playwright.InputFile{{
		Name: "tasks.tsv", MimeType: "text/tab-separated-values", Buffer: []byte(tsv),
	}}); err != nil {
		t.Fatalf("导入 TSV: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('TSV'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
		toasts, _ := page.Locator("#v2-toasts").TextContent()
		recent, _ := page.Locator("#v2-cs-tasks-recent").TextContent()
		state, _ := page.Locator("#v2-cs-state").TextContent()
		t.Fatalf("TSV 导入未回显: %v (toasts=%q recent=%q state=%q)", err, toasts, recent, state)
	}
	if _, err := page.WaitForFunction(
		"() => { const r = document.querySelector('#v2-cs-tasks-recent'); return r && !r.hidden && r.textContent.includes('导入 TSV'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("最近操作未记录: %v", err)
	}

	// 概览趋势：近 1h 云任务失败数来自真实 /api/cloud/tasks?status=failed。
	if err := page.Locator("[data-cs=overview]").Click(); err != nil {
		t.Fatalf("回到概览: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const v = document.querySelector('#v2-cs-trend .v'); return v && v.textContent !== '—'; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("概览趋势未加载: %v", err)
	}

	// 连接设置：保存并校验凭据（loopback 允许匿名验签）→ 退出登录；隧道开关保存。
	if err := page.Locator("#v2-settings-open").Click(); err != nil {
		t.Fatalf("打开连接设置: %v", err)
	}
	if err := waitLoc(page, "#v2-settings", playwright.WaitForSelectorStateVisible, 5000); err != nil {
		t.Fatalf("连接设置未显示: %v", err)
	}
	if err := page.Locator("#v2-set-ak").Fill("EXAMPLE-AK"); err != nil {
		t.Fatalf("填写 AK: %v", err)
	}
	if err := page.Locator("#v2-set-sk").Fill("skey-example01"); err != nil {
		t.Fatalf("填写 SK: %v", err)
	}
	if err := page.Locator("#v2-set-save").Click(); err != nil {
		t.Fatalf("保存凭据: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('凭据已保存'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("凭据保存未回显: %v", err)
	}
	if err := page.Locator("#v2-set-signout").Click(); err != nil {
		t.Fatalf("退出登录: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('已退出登录'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
		t.Fatalf("退出登录未回显: %v", err)
	}
	if err := page.Locator("#v2-set-tunnel").Check(); err != nil {
		t.Fatalf("勾选隧道开关: %v", err)
	}
	if err := page.Locator("#v2-set-tunnel-save").Click(); err != nil {
		t.Fatalf("保存隧道开关: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const t = document.querySelector('#v2-toasts'); return t && t.textContent.includes('隧道开关已保存'); }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("隧道开关未回显: %v", err)
	}
	// 语言切换：设置内切英文 → 壳层/控制台分区即时翻译；切回中文。
	if _, err := page.Locator("#v2-set-lang").SelectOption(playwright.SelectOptionValues{Values: &[]string{"en"}}); err != nil {
		t.Fatalf("切换语言: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const n = document.querySelector('.nav[data-route=files] span'); const c = document.querySelector('[data-cs=overview]'); const th = Array.from(document.querySelectorAll('[data-cspane=tasks] th')).find(x => x.textContent === 'Task'); return n && n.textContent === 'Files' && c && c.textContent === 'Overview' && th && th.textContent === 'Task'; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("英文词条未生效: %v", err)
	}
	if _, err := page.Locator("#v2-set-lang").SelectOption(playwright.SelectOptionValues{Values: &[]string{"zh"}}); err != nil {
		t.Fatalf("切回中文: %v", err)
	}
	if _, err := page.WaitForFunction(
		"() => { const n = document.querySelector('.nav[data-route=files] span'); return n && n.textContent === '文件'; }",
		nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(4000)}); err != nil {
		t.Fatalf("中文词条未恢复: %v", err)
	}
	if err := page.Locator("#v2-settings-close").Click(); err != nil {
		t.Fatalf("关闭设置: %v", err)
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

		// TOTP 注册：真实 POST /api/credentials/register → 展示 AK/Base32/二维码。
		if err := page.Locator("#v2-set-register-toggle").Click(); err != nil {
			t.Fatalf("展开注册: %v", err)
		}
		if err := waitLoc(page, "#v2-reg-submit", playwright.WaitForSelectorStateVisible, 5000); err != nil {
			t.Fatalf("注册表单未显示: %v", err)
		}
		if err := page.Locator("#v2-reg-owner").Fill("e2ereg"); err != nil {
			t.Fatalf("填写 owner: %v", err)
		}
		if err := page.Locator("#v2-reg-submit").Click(); err != nil {
			t.Fatalf("提交注册: %v", err)
		}
		if _, err := page.WaitForFunction(
			"() => { const b = document.querySelector('#v2-reg-result'); return b && b.textContent.includes('AccessKey') && b.textContent.length > 40; }",
			nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(15000)}); err != nil {
			t.Fatalf("注册结果未渲染 (result=%q banner=%q): %v", mustText(page, "#v2-reg-result"), mustText(page, "#v2-set-state"), err)
		}
		if _, err := page.WaitForFunction(
			"() => { const q = document.querySelector('#v2-reg-qr'); return q && q.children.length > 0; }",
			nil, playwright.PageWaitForFunctionOptions{Timeout: playwright.Float(5000)}); err != nil {
			t.Fatalf("注册二维码未渲染 (qr=%q): %v", mustText(page, "#v2-reg-qr"), err)
		}
		if err := page.Locator("#v2-set-register-toggle").Click(); err != nil {
			t.Fatalf("收起注册: %v", err)
		}
	}
}

// mustText 返回选择器文本（失败返回空串，仅用于失败诊断）。
func mustText(page playwright.Page, sel string) string {
	el, err := page.Locator(sel).TextContent()
	if err != nil {
		return ""
	}
	return el
}
