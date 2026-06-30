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
func testServer(t *testing.T) (string, *server.Config, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	cfg := server.Default()
	cfg.UploadsDir = tmpDir
	cfg.LogLevel = "error"

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

	if _, err := page.Evaluate(`() => {
		document.getElementById('token').value = 'test-token-123';
		document.getElementById('save-token-btn').click();
	}`); err != nil {
		t.Fatalf("fill+click: %v", err)
	}

	val, err := page.Evaluate("localStorage.getItem('sproxy_token')")
	if err != nil {
		t.Fatal(err)
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
