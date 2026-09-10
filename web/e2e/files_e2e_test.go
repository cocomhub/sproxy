// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// files_e2e_test.go — 文件类 Web UI 真交互 E2E（PR-F2 任务 1/2）。
//
// 每个用例都走「控件操作 → 捕获并断言网络请求（方法/URL/query/header/body）→
// 断言变化后的 DOM（新行出现 / 旧行消失 / 计数归零）」三要素，杜绝「只断元素存在」
// 的 false-green。dialog（confirm/prompt）类用例一律在点击前装 OnDialog，否则
// Playwright 默认 auto-dismiss 会让点击 inert。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestFiles_Upload 上传链路：选择文件 → POST /upload（校验 X-File-Checksum 与 multipart
// body）→ 列表新增行 + 落盘。
func TestFiles_Upload(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 基线：导航触发的 GET /api/files 返回空列表，且此刻页面**没有** #file-table。
	// 这样「上传后 #file-table 出现」才是真实变化（反 false-green）。
	resp, err := page.ExpectResponse("**/api/files?*", func() error {
		_, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)})
		return gerr
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到导航触发的 GET /api/files: %v", err)
	}
	var listBaseline struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if jerr := resp.JSON(&listBaseline); jerr != nil {
		t.Fatalf("解析基线 /api/files: %v", jerr)
	}
	if len(listBaseline.Files) != 0 {
		t.Fatalf("基线列表应初始为空，实际 %d 项", len(listBaseline.Files))
	}
	if cnt, _ := page.Locator("#file-table").Count(); cnt != 0 {
		t.Fatalf("上传前不应存在 #file-table（count=%d）", cnt)
	}

	// 探针文件（内容已知，用于预算 checksum）。
	probe := filepath.Join(t.TempDir(), "upload-probe.txt")
	content := []byte("upload probe content\n")
	if werr := os.WriteFile(probe, content, 0o644); werr != nil {
		t.Fatal(werr)
	}
	sum := sha256.Sum256(content)
	wantChecksum := hex.EncodeToString(sum[:])

	// 接线①：SetInputFiles 触发 change → uploadFiles → POST /upload。
	req, err := page.ExpectRequest("**/upload", func() error {
		return page.Locator("#file-input").SetInputFiles([]string{probe})
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到 POST /upload（#file-input change 未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("upload method = %q, want POST", got)
	}
	if got := req.Headers()["x-file-checksum"]; got != wantChecksum {
		t.Errorf("X-File-Checksum = %q, want %q（前端未计算/未带上真实 SHA-256）", got, wantChecksum)
	}
	body, err := req.PostDataBuffer()
	if err != nil {
		t.Fatalf("读取上传请求体: %v", err)
	}
	if !bytes.Contains(body, []byte(`name="file"`)) {
		t.Error("multipart body 缺 file 字段")
	}
	if !bytes.Contains(body, []byte(filepath.Base(probe))) {
		t.Error("multipart body 未携带文件名")
	}
	// 默认单卷（未选择卷）→ currentVolume()=="" → 不发送 volume 字段。
	if bytes.Contains(body, []byte(`name="volume"`)) {
		t.Error("未选择卷时不应发送 multipart volume 字段（auto 语义破坏）")
	}

	// 接线②：上传成功后 refreshList 重渲染 → 表格出现且含新行；磁盘真实落盘。
	if werr := waitLoc(page, "#file-table tr", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf("上传后 #file-table 未出现（refreshList 未接线？）: %v", werr)
	}
	txt, err := page.Locator("#file-table").InnerText()
	if err != nil {
		t.Fatalf("读取文件表: %v", err)
	}
	base := filepath.Base(probe)
	if !strings.Contains(txt, base) {
		t.Errorf("上传后文件表未包含 %q；实际:\n%s", base, txt)
	}
	if !fileExists(filepath.Join(userRoot(cfg), base)) {
		t.Errorf("上传未落盘 %s", filepath.Join(userRoot(cfg), base))
	}
}

// TestFiles_Mkdir 新建目录（无 dialog）：填名 → POST /mkdir?dirname= → .dir-row 出现。
func TestFiles_Mkdir(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, "#file-list .empty-msg", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("初始空列表未渲染: %v", err)
	}
	// 基线：无任何目录行（反 false-green——目录行必须是 mkdir 之后新增）。
	if cnt, _ := page.Locator(".dir-row").Count(); cnt != 0 {
		t.Fatalf("mkdir 前不应存在 .dir-row（count=%d）", cnt)
	}

	if err := page.Locator("#new-dir-name").Fill("newdir"); err != nil {
		t.Fatalf("fill new-dir-name: %v", err)
	}

	req, err := page.ExpectRequest("**/mkdir?dirname=*", func() error {
		return page.Locator("#mkdir-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /mkdir（#mkdir-btn 未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("mkdir method = %q, want POST", got)
	}
	if !strings.Contains(req.URL(), "dirname=newdir") {
		t.Errorf("mkdir URL = %q, want 含 dirname=newdir", req.URL())
	}

	// DOM：目录行出现（0→1），面包屑仍在根。
	if werr := waitLoc(page, ".dir-row[data-subdir='newdir']", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf(".dir-row[data-subdir='newdir'] 未出现（refreshList 未接线？）: %v", werr)
	}
	if cnt, _ := page.Locator(".dir-row").Count(); cnt != 1 {
		t.Errorf(".dir-row 计数 = %d, want 1", cnt)
	}
	breadcrumb, _ := page.Locator("#dir-breadcrumb").InnerText()
	if !strings.Contains(breadcrumb, "/") {
		t.Errorf("面包屑应仍在根（含 /），实际 %q", breadcrumb)
	}
}

// TestFiles_Breadcrumb 目录导航：进入子目录 → GET /api/files?subdir=sub → 面包屑与列表
// 切换；点根链接返回 → subdir 清空、子文件消失。
func TestFiles_Breadcrumb(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, userRoot(cfg), "sub/deep.txt", "deep")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".dir-row[data-subdir='sub']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("根目录未渲染 sub 目录行: %v", err)
	}

	// 进入子目录：捕获 refreshList 触发的 GET /api/files，断言 subdir 查询参数。
	resp, err := page.ExpectResponse("**/api/files?*", func() error {
		return page.Locator(".dir-row[data-subdir='sub'] .dir-enter-btn").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击进入目录未触发 GET /api/files: %v", err)
	}
	if !strings.Contains(resp.URL(), "subdir=sub") {
		t.Errorf("进入目录请求 URL = %q, want 含 subdir=sub", resp.URL())
	}

	// DOM：面包屑含 sub，列表含 deep.txt。
	breadcrumb, _ := page.Locator("#dir-breadcrumb").InnerText()
	if !strings.Contains(breadcrumb, "sub") {
		t.Errorf("面包屑未含 sub: %q", breadcrumb)
	}
	if verr := waitLoc(page, "#file-table tr", playwright.WaitForSelectorStateVisible, 8000); verr != nil {
		t.Fatalf("子目录文件表未渲染: %v", verr)
	}
	listTxt, _ := page.Locator("#file-list").InnerText()
	if !strings.Contains(listTxt, "deep.txt") {
		t.Errorf("子目录列表未含 deep.txt:\n%s", listTxt)
	}

	// 返回根：点面包屑根链接（data-subdir="")，断言 subdir 不再为 sub 且子文件消失。
	resp2, err := page.ExpectResponse("**/api/files?*", func() error {
		return page.Locator("#dir-breadcrumb a[data-subdir='']").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击面包屑返回根未触发 GET /api/files: %v", err)
	}
	if strings.Contains(resp2.URL(), "subdir=sub") {
		t.Errorf("返回根请求 URL 仍含 subdir=sub: %q", resp2.URL())
	}
	waitTextGone(t, page, "#file-list", "deep.txt", 8000)
	if werr := waitLoc(page, ".dir-row[data-subdir='sub']", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf("返回根后 sub 目录行未重新出现: %v", werr)
	}
}

// TestFiles_Search 搜索：填关键词 → GET /api/files/search?q= → 列表被搜索结果替换
// （只含匹配项）→ 清除后恢复全量列表。
func TestFiles_Search(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, userRoot(cfg), "search-me.txt", "findable")
	testFile(t, userRoot(cfg), "other.txt", "unrelated")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, "#file-table tr", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("全量列表未渲染: %v", err)
	}
	// 基线：全量列表同时含两个文件。
	listTxt, _ := page.Locator("#file-list").InnerText()
	if !strings.Contains(listTxt, "other.txt") {
		t.Fatalf("基线列表应含 other.txt:\n%s", listTxt)
	}

	if err := page.Locator("#search-input").Fill("search-me"); err != nil {
		t.Fatalf("fill search-input: %v", err)
	}

	req, err := page.ExpectRequest("**/api/files/search?*", func() error {
		return page.Locator("#search-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 GET /api/files/search（#search-btn 未接线？）: %v", err)
	}
	if !strings.Contains(req.URL(), "q=search-me") {
		t.Errorf("搜索 URL = %q, want 含 q=search-me", req.URL())
	}

	// DOM：搜索结果替换全量列表——先等结果渲染出 search-me.txt，再断言 other.txt
	// 不在列表中（证明列表被替换为搜索结果而非仍是全量）。
	waitTextVisible(t, page, "#file-list", "search-me.txt", 8000)
	searchTxt, _ := page.Locator("#file-list").InnerText()
	if strings.Contains(searchTxt, "other.txt") {
		t.Errorf("搜索结果不应含 other.txt（列表未被替换）:\n%s", searchTxt)
	}

	// 清除搜索：按钮隐藏 + 全量列表恢复（other.txt 重新出现）。
	if err := page.Locator("#clear-search-btn").Click(); err != nil {
		t.Fatalf("click clear-search-btn: %v", err)
	}
	waitTextVisible(t, page, "#file-list", "other.txt", 8000)
	if vis, verr := page.Locator("#clear-search-btn").IsVisible(); verr != nil {
		t.Fatalf("读取 clear-search-btn 可见性: %v", verr)
	} else if vis {
		t.Error("清除搜索后 #clear-search-btn 应隐藏（display:none）")
	}
}
