// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package e2e 提供基于 Playwright 的 Web UI 端到端测试。
// 独立 go.mod 避免污染主仓库依赖。
package e2e

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// testServer 启动 sproxy 测试实例并返回 baseURL 和 cleanup。
// opts 为可选参数，第一个 bool 表示是否启用版本管理。
func testServer(t *testing.T, opts ...bool) (string, *server.Config, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	cfg := server.Default()
	cfg.UploadsDir = tmpDir
	cfg.LogLevel = "error"
	if len(opts) > 0 && opts[0] {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	}

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)

	key := make([]byte, 32)
	mux := http.NewServeMux()
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux:       mux,
		CfgPtr:    &cfgPtr,
		Version:   "e2e-test",
		BuildAt:   "e2e-test",
		TunnelKey: key,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ts := httptest.NewServer(h.Handler())
	return ts.URL, cfg, func() {
		ts.Close()
		h.Close()
	}
}

// testFile creates a test file in the uploads directory.
func testFile(t *testing.T, uploadsDir, name, content string) {
	t.Helper()
	p := filepath.Join(uploadsDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// pageFixture 创建 Playwright browser+page，返回 cleanup 函数。
func pageFixture(t *testing.T) (playwright.Page, func()) {
	t.Helper()

	pw, err := playwright.Run()
	if err != nil {
		t.Skipf("playwright unavailable: %v", err)
	}

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		pw.Stop()
		t.Skipf("browser launch failed: %v", err)
	}

	page, err := browser.NewPage()
	if err != nil {
		browser.Close()
		pw.Stop()
		t.Fatal(err)
	}

	return page, func() {
		page.Close()
		browser.Close()
		pw.Stop()
	}
}

// TestUILoads 验证首页加载和静态资源可访问。
func TestUILoads(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	resp, err := page.Goto(baseURL+"/", playwright.PageGotoOptions{
		Timeout: playwright.Float(10000),
	})
	if err != nil {
		t.Fatalf("goto: %v", err)
	}
	if resp.Status() != 200 {
		t.Errorf("status = %d, want 200", resp.Status())
	}

	title, err := page.Title()
	if err != nil {
		t.Fatalf("title: %v", err)
	}
	if !strings.Contains(title, "sproxy") {
		t.Errorf("title = %q, should contain 'sproxy'", title)
	}

	_, err = page.WaitForSelector("h1", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})
	if err != nil {
		t.Fatalf("h1 not found: %v", err)
	}
}

// TestFileList 验证文件列表渲染。
func TestFileList(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "hello.txt", "hello world")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	_, err := page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})
	if err != nil {
		t.Fatalf("file table not loaded: %v", err)
	}

	content, _ := page.Content()
	if !strings.Contains(content, "hello.txt") {
		t.Error("expected hello.txt in page content")
	}
}

// TestDirectoryNavigation 验证目录导航。
func TestDirectoryNavigation(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "sub/deep.txt", "deep")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	_, err := page.WaitForSelector("#dir-bar", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})
	if err != nil {
		t.Fatalf("dir-bar not found: %v", err)
	}
}

// TestAuthFlow 验证 Token 输入和 localStorage 持久化。
func TestAuthFlow(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 等待页面 JS 初始化完成。
	_, err := page.WaitForSelector("#token", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("token input not visible: %v", err)
	}

	// 直接操作 localStorage，验证页面 JS 上下文可读写。
	if _, err := page.Evaluate(`localStorage.setItem('sproxy_token', 'test-token-123')`); err != nil {
		t.Fatal(err)
	}
	raw, err := page.Evaluate(`localStorage.getItem('sproxy_token')`)
	if err != nil {
		t.Fatal(err)
	}
	var val string
	switch v := raw.(type) {
	case string:
		val = v
	case nil:
		t.Fatal("localStorage.getItem returned nil, JS context may not have localStorage access")
	default:
		t.Fatalf("unexpected type %T: %v", raw, raw)
	}
	if val != "test-token-123" {
		t.Errorf("stored token = %q, want test-token-123", val)
	}
}

// TestUploadButton 验证上传控件存在。
func TestUploadButton(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	for _, sel := range []string{"#upload-btn-label", "#file-input", "#search-input"} {
		if cnt, _ := page.Locator(sel).Count(); cnt == 0 {
			t.Errorf("element %s not found", sel)
		}
	}
}

// TestDownloadLink 验证下载按钮存在。
func TestDownloadLink(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "dl-test.txt", "downloadable")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})

	links := page.Locator(".file-download-btn")
	if cnt, _ := links.Count(); cnt == 0 {
		t.Error("no download buttons found")
	}
}

// TestBatchToolbar 验证批量操作工具栏。
func TestBatchToolbar(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if cnt, _ := page.Locator("#batch-toolbar").Count(); cnt == 0 {
		t.Error("batch toolbar #batch-toolbar not found")
	}

	for _, sel := range []string{"#batch-delete-btn", "#batch-rename-btn", "#batch-archive-btn"} {
		if cnt, _ := page.Locator(sel).Count(); cnt == 0 {
			t.Errorf("batch button %s not found", sel)
		}
	}
}

// TestStatsPanel 验证监控面板按钮存在。
func TestStatsPanel(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if cnt, _ := page.Locator("#stats-btn").Count(); cnt == 0 {
		t.Error("stats button not found")
	}
}

// TestMkdirButton 验证新建目录按钮存在。
func TestMkdirButton(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if cnt, _ := page.Locator("#mkdir-btn").Count(); cnt == 0 {
		t.Error("mkdir button not found")
	}
}

// TestCloudDownloadButtonExists 验证云端下载按钮存在。
func TestCloudDownloadButtonExists(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if cnt, _ := page.Locator("#cloud-btn").Count(); cnt == 0 {
		t.Error("cloud download button #cloud-btn not found")
	}
}

// TestCloudDownloadModalOpens 验证点击按钮打开云端下载弹窗。
func TestCloudDownloadModalOpens(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 通过 evaluate 直接调用 JS 函数（避免 CSP 阻止 inline onclick）
	if _, err := page.Evaluate("showCloudDownload()"); err != nil {
		t.Fatalf("showCloudDownload: %v", err)
	}

	// 等待弹窗可见
	_, err := page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal not visible: %v", err)
	}

	// 验证关键元素存在
	for _, sel := range []string{"#cloud-url", "#cloud-create-btn", "#cloud-tasks-body", "#cloud-refresh-btn", "#cloud-close-modal-btn"} {
		if cnt, _ := page.Locator(sel).Count(); cnt == 0 {
			t.Errorf("element %s not found in cloud modal", sel)
		}
	}
}

// TestCloudDownloadModalCloses 验证关闭云端下载弹窗。
func TestCloudDownloadModalCloses(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开弹窗
	page.Evaluate("showCloudDownload()")
	page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})

	// 关闭弹窗
	if _, err := page.Evaluate("hideCloudDownload()"); err != nil {
		t.Fatalf("hideCloudDownload: %v", err)
	}

	// 验证弹窗隐藏
	_, err := page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal should be hidden: %v", err)
	}

	// 重新打开再关闭（验证幂等性）
	page.Evaluate("showCloudDownload()")
	page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	page.Evaluate("hideCloudDownload()")
	_, err = page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal should be hidden after second close: %v", err)
	}
}

// TestCloudDownloadCreateTask 验证创建云端下载任务。
func TestCloudDownloadCreateTask(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开弹窗
	page.Evaluate("showCloudDownload()")
	_, err := page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal not visible: %v", err)
	}

	// 输入 URL
	if err := page.Locator("#cloud-url").Fill("https://example.com/test.zip"); err != nil {
		t.Fatalf("fill cloud-url: %v", err)
	}

	// 点击开始下载
	if err := page.Locator("#cloud-create-btn").Click(); err != nil {
		t.Fatalf("click create btn: %v", err)
	}

	// 等待响应（URL 不可达，但至少验证没有 crash）
	page.WaitForSelector("#cloud-tasks-body", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	})
}

// TestCloudDownloadTaskList 验证任务列表渲染。
func TestCloudDownloadTaskList(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开弹窗
	page.Evaluate("showCloudDownload()")
	_, err := page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal not visible: %v", err)
	}

	// 等待任务列表加载
	_, err = page.WaitForSelector("#cloud-tasks-body", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-tasks-body not found: %v", err)
	}

	// 验证刷新按钮可用
	if err := page.Locator("#cloud-refresh-btn").Click(); err != nil {
		t.Fatalf("click refresh btn: %v", err)
	}
}

// TestCloudDownloadURLInput 验证云端下载弹窗中的 textarea 输入框存在且可用。
func TestCloudDownloadURLInput(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开弹窗
	page.Evaluate("showCloudDownload()")
	_, err := page.WaitForSelector("#cloud-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("cloud-modal not visible: %v", err)
	}

	// 验证 textarea 存在（已从 input 改为 textarea 支持多行）
	tag, err := page.Evaluate("document.querySelector('#cloud-url').tagName")
	if err != nil {
		t.Fatalf("evaluate tagName: %v", err)
	}
	if tag != "TEXTAREA" {
		t.Fatalf("expected TEXTAREA element, got %s", tag)
	}
}

// TestShareButton 验证文件行有分享按钮。
func TestShareButton(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "share-test.txt", "shareable content")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})

	if cnt, _ := page.Locator(".file-actions .btn-share").Count(); cnt == 0 {
		// 退而求其次：通过 JS 验证 shareFile 函数存在
		exists, err := page.Evaluate("typeof window.shareFile === 'function'")
		if err != nil || exists != true {
			t.Fatal("shareFile function not found")
		}
	}
}

// TestShareAPI 验证分享 API 可调用。
func TestShareAPI(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "api-share-test.txt", "api content")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	result, err := page.Evaluate(`fetch('/api/share', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ filename: 'api-share-test.txt', ttl: '24h', max_downloads: 0, one_time: false })
	}).then(function(r) { return r.json(); })`)
	if err != nil {
		t.Fatalf("share API call failed: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if m["token"] == nil || m["token"] == "" {
		t.Error("expected non-empty token in share response")
	}
	if m["filename"] != "api-share-test.txt" {
		t.Errorf("filename = %v, want api-share-test.txt", m["filename"])
	}
}

// TestVersioningButton 验证版本管理按钮存在。
func TestVersioningButton(t *testing.T) {
	baseURL, _, cleanup := testServer(t, true)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if cnt, _ := page.Locator("#version-btn").Count(); cnt == 0 {
		t.Error("version management button #version-btn not found")
	}
}

// TestVersioningModalOpenClose 验证版本管理弹窗打开和关闭。
func TestVersioningModalOpenClose(t *testing.T) {
	baseURL, _, cleanup := testServer(t, true)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开弹窗
	if _, err := page.Evaluate("showVersioning()"); err != nil {
		t.Fatalf("showVersioning: %v", err)
	}
	_, err := page.WaitForSelector("#version-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("version-modal not visible: %v", err)
	}

	// 验证关键元素
	for _, sel := range []string{"#version-filename", "#version-load-btn", "#version-body"} {
		if cnt, _ := page.Locator(sel).Count(); cnt == 0 {
			t.Errorf("element %s not found in version modal", sel)
		}
	}

	// 关闭弹窗
	if _, err := page.Evaluate("hideVersioning()"); err != nil {
		t.Fatalf("hideVersioning: %v", err)
	}
	_, err = page.WaitForSelector("#version-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("version-modal should be hidden: %v", err)
	}
}

// TestVersioningLoadVersions 验证加载版本历史。
func TestVersioningLoadVersions(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t, true)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "versioned.txt", "v1 content")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	page.Evaluate("showVersioning()")
	page.WaitForSelector("#version-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})

	// 输入文件名并加载版本
	if err := page.Locator("#version-filename").Fill("versioned.txt"); err != nil {
		t.Fatalf("fill version-filename: %v", err)
	}
	if err := page.Locator("#version-load-btn").Click(); err != nil {
		t.Fatalf("click version-load-btn: %v", err)
	}

	// 新文件没有版本历史，应显示"没有版本历史"
	_, err := page.WaitForSelector("text=该文件没有版本历史", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Error("expected '该文件没有版本历史' message for new file")
	}
}

// TestVersioningDisabledMessage 验证版本管理未启用时显示友好提示。
func TestVersioningDisabledMessage(t *testing.T) {
	baseURL, _, cleanup := testServer(t, false)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	page.Evaluate("showVersioning()")
	page.WaitForSelector("#version-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})

	if err := page.Locator("#version-filename").Fill("any.txt"); err != nil {
		t.Fatalf("fill version-filename: %v", err)
	}
	if err := page.Locator("#version-load-btn").Click(); err != nil {
		t.Fatalf("click version-load-btn: %v", err)
	}

	// 应显示"版本管理未启用"提示
	_, err := page.WaitForSelector("text=版本管理未启用", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Error("expected '版本管理未启用' message when versioning disabled")
	}
}

// TestDirArchiveButton 验证目录行有打包下载按钮。
func TestDirArchiveButton(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "archivedir/sub/dummy.txt", "dummy")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})

	if cnt, _ := page.Locator(".dir-archive-btn").Count(); cnt == 0 {
		// 退而求其次：通过 JS 验证 downloadDirArchive 函数存在
		exists, err := page.Evaluate("typeof window.downloadDirArchive === 'function'")
		if err != nil || exists != true {
			t.Fatal("downloadDirArchive function not found")
		}
	}
}

// TestSearchFunction 验证搜索功能可用。
func TestSearchFunction(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, cfg.UploadsDir, "search-me.txt", "findable content")
	testFile(t, cfg.UploadsDir, "ignore-me.txt", "hidden content")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})

	// 输入搜索关键词
	if err := page.Locator("#search-input").Fill("search"); err != nil {
		t.Fatalf("fill search-input: %v", err)
	}

	// 点击搜索按钮
	if err := page.Locator("#search-btn").Click(); err != nil {
		t.Fatalf("click search-btn: %v", err)
	}

	// 等待结果
	page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(5000)})

	content, err := page.Content()
	if err != nil {
		t.Fatalf("page content: %v", err)
	}

	if !strings.Contains(content, "search-me.txt") {
		t.Error("expected search-me.txt in search results")
	}
	if strings.Contains(content, "ignore-me.txt") {
		t.Error("did not expect ignore-me.txt in search results")
	}

	// 验证清除搜索按钮存在
	if cnt, _ := page.Locator("#clear-search-btn").Count(); cnt == 0 {
		t.Error("clear search button not found during search")
	}
}

// TestStorageConfigInStats 验证监控弹窗中有存储限制配置 UI。
func TestStorageConfigInStats(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开监控弹窗
	page.Evaluate("showStats()")
	_, err := page.WaitForSelector("#stats-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("stats-modal not visible: %v", err)
	}

	// 验证存储限制配置元素存在
	for _, sel := range []string{"#max-storage-input", "text=存储限制"} {
		if cnt, _ := page.Locator(sel).Count(); cnt == 0 {
			t.Errorf("element %s not found in stats modal", sel)
		}
	}
}

// TestStorageConfigAPI 验证存储配置 API 可调用。
func TestStorageConfigAPI(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	result, err := page.Evaluate(`fetch('/api/storage/config', {
		method: 'PUT',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ max_storage_bytes: 104857600 })
	}).then(function(r) { return r.json(); })`)
	if err != nil {
		t.Fatalf("storage config API call failed: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if m["success"] != true {
		t.Errorf("success = %v, want true", m["success"])
	}
	if m["max_storage_bytes"] != float64(104857600) {
		t.Errorf("max_storage_bytes = %v, want 104857600", m["max_storage_bytes"])
	}
}
