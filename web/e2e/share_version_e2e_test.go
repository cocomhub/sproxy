// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// share_version_e2e_test.go — 分享 / 公链 / 版本管理 Web UI 真交互 E2E（PR-F2 任务 3）。
//
// 分享链路：UI 创建（POST /api/share，断言 body 四字段）→ 列表行取 token → Go 侧直取
// /s/{token} 断言字节与 Content-Disposition → UI 撤销（DELETE /api/shares/{token}）。
// 版本链路：同文件名两次真上传产版本 → UI 加载（GET /api/versions）→ 表格行（非
// .empty-msg 假绿）→ 恢复（POST /api/versions/restore）。

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestShare_CreateAndPublicAccess 分享创建 → 公链取字节 → 撤销。
func TestShare_CreateAndPublicAccess(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	sharedBody := []byte("shareable body")
	if status, body := seedUploadToVolume(t, baseURL, "default", "share-src.txt", sharedBody); status != http.StatusOK {
		t.Fatalf("seed share src status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".file-share-btn[data-filename='share-src.txt']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("分享按钮未渲染: %v", err)
	}

	// 打开分享弹窗：文件名被预填（showShareModal 接线）。
	if err := page.Locator(".file-share-btn[data-filename='share-src.txt']").Click(); err != nil {
		t.Fatalf("click share btn: %v", err)
	}
	if err := waitLoc(page, "#share-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("分享弹窗未打开: %v", err)
	}
	if v, verr := page.Locator("#share-filename").InputValue(); verr != nil {
		t.Fatalf("读取 share-filename: %v", verr)
	} else if v != "share-src.txt" {
		t.Fatalf("#share-filename = %q, want share-src.txt（showShareModal 未预填文件名）", v)
	}

	// 创建：POST /api/share，body 四字段逐项断言。
	req, err := page.ExpectRequest("**/api/share", func() error {
		return page.Locator("#share-create-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/share（分享创建未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("share method = %q, want POST", got)
	}
	var shareReq struct {
		Filename     string `json:"filename"`
		TTL          string `json:"ttl"`
		MaxDownloads int    `json:"max_downloads"`
		OneTime      bool   `json:"one_time"`
	}
	requestJSON(t, req, &shareReq)
	if shareReq.Filename != "share-src.txt" || shareReq.TTL != "24h" || shareReq.MaxDownloads != 0 || shareReq.OneTime {
		t.Fatalf("share body = %+v, want {share-src.txt 24h 0 false}", shareReq)
	}

	// 列表行出现 → 取 token（复制按钮 data-token）。
	if werr := waitLoc(page, "#share-list-body .share-copy-btn", playwright.WaitForSelectorStateAttached, 8000); werr != nil {
		t.Fatalf("分享列表未渲染 token 行（refreshShareList 未接线？）: %v", werr)
	}
	token, err := page.Locator("#share-list-body .share-copy-btn").First().GetAttribute("data-token")
	if err != nil {
		t.Fatalf("读取分享 token: %v", err)
	}
	if token == "" {
		t.Fatal("分享 token 为空")
	}

	// 公链（Go 侧最强行证）：GET /s/{token} 无认证返回文件字节 + attachment。
	sresp, err := http.Get(baseURL + "/s/" + token)
	if err != nil {
		t.Fatalf("公链请求失败: %v", err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("公链状态 = %d, want 200", sresp.StatusCode)
	}
	if cd := sresp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want 含 attachment", cd)
	}
	gotBody, err := io.ReadAll(sresp.Body)
	if err != nil {
		t.Fatalf("读取公链响应体: %v", err)
	}
	if string(gotBody) != string(sharedBody) {
		t.Errorf("公链返回字节 = %q, want %q", gotBody, sharedBody)
	}

	// 撤销：切到「管理分享」面板（列表行可见才可点）→ confirm → DELETE /api/shares/{token}。
	if tabErr := page.Locator("#share-list-tab").Click(); tabErr != nil {
		t.Fatalf("切换到管理分享面板: %v", tabErr)
	}
	acceptDialog(page, "")
	req2, err := page.ExpectRequest("**/api/shares/*", func() error {
		return page.Locator("#share-list-body .share-revoke-btn").First().Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 DELETE /api/shares/{token}（撤销未接线？）: %v", err)
	}
	if got := req2.Method(); got != "DELETE" {
		t.Errorf("revoke method = %q, want DELETE", got)
	}
	if !strings.Contains(req2.URL(), token) {
		t.Errorf("revoke URL = %q, want 含 token %q", req2.URL(), token)
	}

	// DOM：撤销后列表回到空态。
	waitTextVisible(t, page, "#share-list-body", "暂无分享链接", 8000)
}

// TestVersioning_UploadCreatesVersions 两次真上传产版本 → UI 加载 → 表格行 → 恢复请求。
func TestVersioning_UploadCreatesVersions(t *testing.T) {
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {
		c.Versioning.Enabled = true
		c.Versioning.MaxVersions = 10
	})
	defer cleanup()

	// 覆盖写产版本：同文件名两次真上传（不得直写 testFile 绕过 upload——否则无版本）。
	// 必须走 auto 路由（vol=""，不带 volume 字段）——显式 volume 会命中卷唯一性查重 409
	// 而拒绝同名覆盖；auto 路由才进入 handleDuplicateFile 的版本化覆盖写分支。
	if status, body := seedUploadMultipart(t, baseURL, "", "versioned.txt", []byte("v1 content")); status != http.StatusOK {
		t.Fatalf("seed v1 status=%d body=%s", status, body)
	}
	if status, body := seedUploadMultipart(t, baseURL, "", "versioned.txt", []byte("v2 content")); status != http.StatusOK {
		t.Fatalf("seed v2 status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#version-btn").Click(); err != nil {
		t.Fatalf("click version-btn: %v", err)
	}
	if err := waitLoc(page, "#version-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("版本弹窗未打开: %v", err)
	}
	// 反 false-green：初始化占位文案存在，加载后必须被表格替换。
	placeholder, _ := page.Locator("#version-body").InnerText()
	if !strings.Contains(placeholder, "输入文件名查看版本历史") {
		t.Fatalf("版本弹窗初始占位缺失: %q", placeholder)
	}

	if err := page.Locator("#version-filename").Fill("versioned.txt"); err != nil {
		t.Fatalf("fill version-filename: %v", err)
	}

	resp, err := page.ExpectResponse("**/api/versions?*", func() error {
		return page.Locator("#version-load-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 GET /api/versions（加载未接线？）: %v", err)
	}
	if got := resp.Status(); got != http.StatusOK {
		t.Fatalf("GET /api/versions status = %d, want 200", got)
	}
	var verPayload struct {
		Versions []struct {
			VersionID int `json:"version_id"`
		} `json:"versions"`
	}
	if jerr := resp.JSON(&verPayload); jerr != nil {
		t.Fatalf("解析版本响应: %v", jerr)
	}
	if len(verPayload.Versions) < 1 {
		t.Fatalf("覆盖写后版本数 = %d, want >= 1（覆盖写未产版本？）", len(verPayload.Versions))
	}

	// DOM（非 .empty-msg）：表格行 + 恢复按钮 + 「共 N 个版本」头部文案。
	if werr := waitLoc(page, "#version-body table tbody tr", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		body, _ := page.Locator("#version-body").InnerText()
		t.Fatalf("版本表格未渲染（loadVersions 未接线？）: %v；body=%q", werr, body)
	}
	if cnt, cerr := page.Locator("#version-body .version-restore-btn").Count(); cerr != nil || cnt < 1 {
		t.Fatalf("版本恢复按钮数 = %d (err=%v), want >= 1", cnt, cerr)
	}
	bodyText, _ := page.Locator("#version-body").InnerText()
	if !strings.Contains(bodyText, "共 ") {
		t.Errorf("版本表头文案缺「共 N 个版本」:\n%s", bodyText)
	}

	// 恢复：confirm → POST /api/versions/restore?filename=&version_id=。
	acceptDialog(page, "")
	req, err := page.ExpectRequest("**/api/versions/restore?*", func() error {
		return page.Locator("#version-body .version-restore-btn").First().Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/versions/restore（恢复未接线？）: %v", err)
	}
	if u := req.URL(); !strings.Contains(u, "filename=versioned.txt") || !strings.Contains(u, "version_id=") {
		t.Errorf("restore URL = %q, want 含 filename=versioned.txt 与 version_id=", u)
	}
	if u := req.URL(); strings.Contains(u, "version_id=&") || strings.HasSuffix(u, "version_id=") {
		t.Errorf("restore URL = %q, version_id 为空", u)
	}
}

// TestVersioning_DisabledReturns501 版本管理未启用：GET /api/versions 返回 501，
// DOM 展示「加载失败」错误占位且不渲染表格（以网络状态码为主证据）。
func TestVersioning_DisabledReturns501(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#version-btn").Click(); err != nil {
		t.Fatalf("click version-btn: %v", err)
	}
	if err := waitLoc(page, "#version-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("版本弹窗未打开: %v", err)
	}
	if err := page.Locator("#version-filename").Fill("any.txt"); err != nil {
		t.Fatalf("fill version-filename: %v", err)
	}

	resp, err := page.ExpectResponse("**/api/versions?*", func() error {
		return page.Locator("#version-load-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 GET /api/versions: %v", err)
	}
	if got := resp.Status(); got != http.StatusNotImplemented {
		t.Fatalf("版本禁用时 status = %d, want 501", got)
	}

	// DOM：错误占位（loadVersions catch 分支），且不渲染版本表格。
	waitTextVisible(t, page, "#version-body", "加载失败", 8000)
	if cnt, _ := page.Locator("#version-body table").Count(); cnt != 0 {
		t.Errorf("版本禁用时不应渲染版本表格（table count=%d）", cnt)
	}
}
