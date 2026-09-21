// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// events_e2e_test.go —— WebUI 文件变更事件流（SSE）实时刷新 E2E。
//
// 验证「上传/删除文件后列表**不手动刷新**也自动更新」：
//   1. 页面加载 → 建立 /api/events SSE 订阅（fetch 流式读 + Last-Event-ID 回放）
//   2. 外部（HTTP 客户端）上传新文件 → 服务端 EventBus 广播 upload 事件
//   3. 断言页面文件表**自动**出现新文件（无任何刷新按钮点击）
//   4. 外部删除文件 → 断言列表自动消失
//
// 无凭据场景（testServer 默认）：服务端 AllowInsecureLoopback=true 回环兜底放行
// /api/events（GET 无签名头）；事件 owner = anonymous（与页面列表请求一致）。
// 认证态（有凭据）场景由 events.js 的签名头 + 服务端 events_test.go（#433）覆盖，
// 此处聚焦「事件驱动自动刷新」这一 Web UI 验收核心。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/mxschmitt/playwright-go"
)

// ---- 本文件辅助（e2e 包自建，参考既有 helper 模式） ----

// playwrightPageGotoOptions 返回默认页面导航参数。
func playwrightPageGotoOptions() playwright.PageGotoOptions {
	return playwright.PageGotoOptions{Timeout: playwright.Float(10000)}
}

// watchEventsRequested 安装 /api/events 请求监听（**必须在导航前调用**——
// SSE 连接在页面加载时随 eventsStart 发出），返回检测函数。
func watchEventsRequested(page playwright.Page) func() bool {
	requested := false
	page.OnRequest(func(req playwright.Request) {
		if strings.Contains(req.URL(), "/api/events") {
			requested = true
		}
	})
	return func() bool { return requested }
}

// uploadFileToVolume 外部 multipart 上传（与服务端同一写路径，触发 EventBus 广播）。
func uploadFileToVolume(t *testing.T, baseURL, filename string, content []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, werr := fw.Write(content); werr != nil {
		t.Fatalf("write file body: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	sum := sha256.Sum256(content)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", hex.EncodeToString(sum[:]))
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("seed upload request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read upload response: %v", err)
	}
	return resp.StatusCode, string(body)
}

// deleteFileToVolume 外部删除（checksum 匹配）。
func deleteFileToVolume(t *testing.T, baseURL, filename, checksum string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/delete?filename="+url.QueryEscape(filename), nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	req.Header.Set("X-File-Checksum", checksum)
	resp, err := testutil.IsolatedClient(t).Do(req)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read delete response: %v", err)
	}
	return resp.StatusCode, string(body)
}

// sha256Hex 返回内容的 SHA-256 hex（checksum 计算）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestEvents_AutoRefreshAfterUpload 上传后列表不手动刷新也自动出现新文件。
// 核心断言链：
//   - 导航后页面建立 SSE 订阅（捕获 /api/events 请求发出）
//   - 外部 multipart 上传 → 服务端事件流广播 upload 事件
//   - 页面文件表自动新增该文件（不点击任何刷新按钮）
func TestEvents_AutoRefreshAfterUpload(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 导航前装监听（SSE 请求随页面加载发出）。
	eventsRequested := watchEventsRequested(page)

	// 导航 → 页面加载（refreshList 首拉空列表 + 顶层 eventsStart 启动事件流）。
	_, gerr := page.Goto(baseURL+"/ui/", playwrightPageGotoOptions())
	if gerr != nil {
		t.Fatalf("goto: %v", gerr)
	}
	if lerr := waitLoc(page, "#file-list", nil, 8000); lerr != nil {
		t.Fatalf("file-list not found: %v", lerr)
	}

	// 断言 SSE 订阅请求已发出（页面建立 /api/events 连接）。
	if !testutil.WaitForBool(8*time.Second, eventsRequested) {
		t.Fatal("页面未发出 /api/events 订阅请求（事件流未启动）")
	}

	// 外部上传新文件（真实 multipart，与服务端同一写路径 → 触发 EventBus 广播）。
	probe := filepath.Join(t.TempDir(), "sse-auto-upload.txt")
	content := []byte("sse auto refresh content\n")
	if werr := os.WriteFile(probe, content, 0o644); werr != nil {
		t.Fatal(werr)
	}
	if status, body := uploadFileToVolume(t, baseURL, probe, content); status != http.StatusOK {
		t.Fatalf("seed upload status=%d body=%s", status, body)
	}

	// 断言：不点击任何刷新按钮，文件表自动出现新文件（SSE 事件驱动 refreshList）。
	if lerr := waitLoc(page, "#file-table tr", nil, 8000); lerr != nil {
		t.Fatalf("上传后文件表未自动出现新行（SSE 实时刷新未生效）: %v", lerr)
	}
	txt, terr := page.Locator("#file-table").InnerText()
	if terr != nil {
		t.Fatalf("读取文件表: %v", terr)
	}
	if !strings.Contains(txt, "sse-auto-upload.txt") {
		t.Fatalf("文件表未自动包含新上传文件; 实际:\n%s", txt)
	}
}

// TestEvents_AutoRefreshAfterDelete 外部删除文件 → 列表自动消失（SSE delete 事件）。
// 先直写磁盘种子文件（列表初始可见），再外部删除 → 事件流广播 delete → 自动刷新移除。
func TestEvents_AutoRefreshAfterDelete(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	// 种子文件直写磁盘（与既有 testFile 相同方式），初始列表可见。
	testFile(t, userRoot(cfg), "sse-auto-delete.txt", "delete me")

	page, stop := pageFixture(t)
	defer stop()

	// 导航前装监听（SSE 请求随页面加载发出）。
	eventsRequested := watchEventsRequested(page)

	_, err := page.Goto(baseURL+"/ui/", playwrightPageGotoOptions())
	if err != nil {
		t.Fatalf("goto: %v", err)
	}
	// 初始列表含种子文件。
	if err := waitLoc(page, "#file-table tr", nil, 8000); err != nil {
		t.Fatalf("种子文件未显示: %v", err)
	}
	if !testutil.WaitForBool(8*time.Second, eventsRequested) {
		t.Fatal("页面未发出 /api/events 订阅请求")
	}

	// 外部删除（文件 + checksum 匹配）。
	sum := sha256Hex("delete me")
	if status, body := deleteFileToVolume(t, baseURL, "sse-auto-delete.txt", sum); status != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", status, body)
	}

	// 断言：不点击刷新，列表自动移除该文件（SSE delete 事件 → refreshList）。
	if !testutil.WaitForBool(8*time.Second, func() bool {
		txt, _ := page.Locator("#file-table").InnerText()
		return !strings.Contains(txt, "sse-auto-delete.txt")
	}) {
		txt, _ := page.Locator("#file-table").InnerText()
		t.Fatalf("删除后文件表仍含已删文件（SSE 实时刷新未生效）; 实际:\n%s", txt)
	}
}
