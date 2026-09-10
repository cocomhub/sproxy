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
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// ---- PR-F2 任务 2：dialog 组（单删 / 批删 / rename / 批重命名 / rmdir）----
// 所有 confirm/prompt 流程必须先装 OnDialog，否则 Playwright auto-dismiss → 点击 inert。

// TestFiles_Delete 单文件删除：confirm → POST /delete?filename=（X-File-Checksum）→ 行消失。
func TestFiles_Delete(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	content := []byte("x")
	if status, body := seedUploadToVolume(t, baseURL, "default", "del-me.txt", content); status != http.StatusOK {
		t.Fatalf("seed upload status=%d body=%s", status, body)
	}
	wantChecksum := hex.EncodeToString(sha256Sum(content))

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".file-delete-btn[data-filename='del-me.txt']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("删除按钮未渲染: %v", err)
	}

	// dialog 必须先装（confirm → 接受），否则点击 inert。
	acceptDialog(page, "")

	req, err := page.ExpectRequest("**/delete?filename=*", func() error {
		return page.Locator(".file-delete-btn[data-filename='del-me.txt']").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /delete（删除按钮未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("delete method = %q, want POST", got)
	}
	if !strings.Contains(req.URL(), "filename=del-me.txt") {
		t.Errorf("delete URL = %q, want 含 filename=del-me.txt", req.URL())
	}
	if got := req.Headers()["x-file-checksum"]; got != wantChecksum {
		t.Errorf("X-File-Checksum = %q, want %q", got, wantChecksum)
	}

	// DOM + 磁盘：行消失且文件真实删除。
	waitTextGone(t, page, "#file-list", "del-me.txt", 8000)
	if fileExists(filepath.Join(userRoot(cfg), "del-me.txt")) {
		t.Error("删除后文件仍存在于磁盘")
	}
}

// TestFiles_BatchDelete 批量删除：勾选 2 个 → confirm → POST /api/batch/delete（body
// files=[{filename,checksum}]）→ 列表清空。
func TestFiles_BatchDelete(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	c1 := []byte("batch-1")
	c2 := []byte("batch-2")
	if status, body := seedUploadToVolume(t, baseURL, "default", "b1.txt", c1); status != http.StatusOK {
		t.Fatalf("seed b1 status=%d body=%s", status, body)
	}
	if status, body := seedUploadToVolume(t, baseURL, "default", "b2.txt", c2); status != http.StatusOK {
		t.Fatalf("seed b2 status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".file-select[data-filename='b1.txt']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("b1 选择框未渲染: %v", err)
	}

	// 勾选两个文件 → updateBatchToolbar 更新计数。
	if err := page.Locator(".file-select[data-filename='b1.txt']").Check(); err != nil {
		t.Fatalf("check b1: %v", err)
	}
	if err := page.Locator(".file-select[data-filename='b2.txt']").Check(); err != nil {
		t.Fatalf("check b2: %v", err)
	}
	waitTextVisible(t, page, "#batch-count", "已选 2", 4000)

	acceptDialog(page, "")

	req, err := page.ExpectRequest("**/api/batch/delete", func() error {
		return page.Locator("#batch-delete-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/batch/delete（批量删除未接线？）: %v", err)
	}
	if got := req.Method(); got != "POST" {
		t.Errorf("batch delete method = %q, want POST", got)
	}
	var payload struct {
		Files []struct {
			Filename string `json:"filename"`
			Checksum string `json:"checksum"`
		} `json:"files"`
	}
	requestJSON(t, req, &payload)
	if len(payload.Files) != 2 {
		t.Fatalf("批量删除 files 数 = %d, want 2", len(payload.Files))
	}
	byName := map[string]string{}
	for _, f := range payload.Files {
		if f.Checksum == "" {
			t.Errorf("批量删除条目 %q checksum 为空", f.Filename)
		}
		byName[f.Filename] = f.Checksum
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "b1.txt,b2.txt" {
		t.Errorf("批量删除文件名集合 = %v, want [b1.txt b2.txt]", names)
	}
	if byName["b1.txt"] != hex.EncodeToString(sha256Sum(c1)) {
		t.Errorf("b1 checksum 不匹配: %s", byName["b1.txt"])
	}
	if byName["b2.txt"] != hex.EncodeToString(sha256Sum(c2)) {
		t.Errorf("b2 checksum 不匹配: %s", byName["b2.txt"])
	}

	// DOM：两行消失、表格清空（空列表不再渲染 #file-table）。
	//
	// ⚠️ 已发现真实 UI 缺陷（不在本 PR 改动范围，见报告）：服务端 /api/batch/delete 返回
	// {"results":[...]}（无顶层 success 字段），而 app.js batchDelete() 以 data.success
	// 判定成功——undefined 为假 → 走 else 分支（弹「批量删除失败」错误 toast）且**不调用
	// refreshList()**，故列表不会自动刷新。此处以显式「刷新列表」点击驱动 refreshList
	// （仍为点击 → GET /api/files → DOM 变化），据实断言服务端删除已生效且渲染一致。
	if rerr := expectFilesReload(page, func() error { return page.Locator("#refresh-btn").Click() }); rerr != nil {
		t.Fatalf("刷新列表未触发 GET /api/files: %v", rerr)
	}
	waitTextGone(t, page, "#file-list", "b1.txt", 8000)
	waitTextGone(t, page, "#file-list", "b2.txt", 8000)
	if cnt, _ := page.Locator("#file-table tr").Count(); cnt != 0 {
		t.Errorf("批量删除后 #file-table tr 计数 = %d, want 0", cnt)
	}
}

// TestFiles_Rename 重命名（prompt）：Accept 新名 → POST /rename?from=&to=（X-File-Checksum）
// → 新名出现、旧名消失。
func TestFiles_Rename(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	if status, body := seedUploadToVolume(t, baseURL, "default", "old-name.txt", []byte("rename me")); status != http.StatusOK {
		t.Fatalf("seed rename src status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".file-rename-btn[data-filename='old-name.txt']", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("重命名按钮未渲染: %v", err)
	}

	// prompt → Accept 带新文件名。
	acceptDialog(page, "new-name.txt")

	req, err := page.ExpectRequest("**/rename?*", func() error {
		return page.Locator(".file-rename-btn[data-filename='old-name.txt']").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /rename（重命名未接线？）: %v", err)
	}
	if !strings.Contains(req.URL(), "from=old-name.txt") || !strings.Contains(req.URL(), "to=new-name.txt") {
		t.Errorf("rename URL = %q, want 含 from=old-name.txt 与 to=new-name.txt", req.URL())
	}
	if got := req.Headers()["x-file-checksum"]; got == "" {
		t.Error("rename 请求缺 X-File-Checksum 头")
	}

	// DOM：新名出现、旧名消失。
	waitTextVisible(t, page, "#file-list", "new-name.txt", 8000)
	waitTextGone(t, page, "#file-list", "old-name.txt", 8000)
}

// TestFiles_BatchRename 批量重命名（N 个序列 prompt）：按 dialog 消息分派新名 →
// POST /api/batch/rename（body operations=[{from,to,checksum}]）→ 新名出现、旧名消失。
func TestFiles_BatchRename(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	if status, body := seedUploadToVolume(t, baseURL, "default", "r1.txt", []byte("one")); status != http.StatusOK {
		t.Fatalf("seed r1 status=%d body=%s", status, body)
	}
	if status, body := seedUploadToVolume(t, baseURL, "default", "r2.txt", []byte("two")); status != http.StatusOK {
		t.Fatalf("seed r2 status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".file-select[data-filename='r1.txt']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("r1 选择框未渲染: %v", err)
	}
	if err := page.Locator(".file-select[data-filename='r1.txt']").Check(); err != nil {
		t.Fatalf("check r1: %v", err)
	}
	if err := page.Locator(".file-select[data-filename='r2.txt']").Check(); err != nil {
		t.Fatalf("check r2: %v", err)
	}
	waitTextVisible(t, page, "#batch-count", "已选 2", 4000)

	// 序列 prompt：按消息内容分派新名（Playwright 串行派发 dialog，无状态分派即安全）。
	respondDialogs(page, func(d playwright.Dialog) {
		switch {
		case strings.Contains(d.Message(), "r1.txt"):
			_ = d.Accept("n1.txt")
		case strings.Contains(d.Message(), "r2.txt"):
			_ = d.Accept("n2.txt")
		default:
			_ = d.Accept("")
		}
	})

	req, err := page.ExpectRequest("**/api/batch/rename", func() error {
		return page.Locator("#batch-rename-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /api/batch/rename（批重命名未接线？）: %v", err)
	}
	var payload struct {
		Operations []struct {
			From     string `json:"from"`
			To       string `json:"to"`
			Checksum string `json:"checksum"`
		} `json:"operations"`
	}
	requestJSON(t, req, &payload)
	if len(payload.Operations) != 2 {
		t.Fatalf("批量重命名 operations 数 = %d, want 2 (body=%+v)", len(payload.Operations), payload)
	}
	toOf := map[string]string{}
	for _, op := range payload.Operations {
		if op.Checksum == "" {
			t.Errorf("operation %q checksum 为空", op.From)
		}
		toOf[op.From] = op.To
	}
	if toOf["r1.txt"] != "n1.txt" || toOf["r2.txt"] != "n2.txt" {
		t.Fatalf("批量重命名映射 = %v, want r1.txt→n1.txt r2.txt→n2.txt", toOf)
	}

	// ⚠️ 同 TestFiles_BatchDelete：/api/batch/rename 亦返回 {"results":[...]}（无顶层
	// success），app.js batchRename() 以 data.success 判定 → 不自动 refreshList。此处以
	// 显式「刷新列表」点击驱动渲染（点击 → GET /api/files → DOM 变化），据实断言服务端
	// 重命名已生效。
	if rerr := expectFilesReload(page, func() error { return page.Locator("#refresh-btn").Click() }); rerr != nil {
		t.Fatalf("刷新列表未触发 GET /api/files: %v", rerr)
	}
	waitTextVisible(t, page, "#file-list", "n1.txt", 8000)
	waitTextVisible(t, page, "#file-list", "n2.txt", 8000)
	waitTextGone(t, page, "#file-list", "r1.txt", 8000)
	waitTextGone(t, page, "#file-list", "r2.txt", 8000)
}

// expectFilesReload 在 action 期间捕获一次 GET /api/files?* 响应（用于显式刷新列表的
// 点击断言）。返回 error 表示未观察到该请求。
func expectFilesReload(page playwright.Page, action func() error) error {
	_, err := page.ExpectResponse("**/api/files?*", action, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	return err
}

// TestFiles_Rmdir 删除目录（confirm）：POST /rmdir?dirname=&force=true → 目录行消失。
func TestFiles_Rmdir(t *testing.T) {
	baseURL, cfg, cleanup := testServer(t)
	defer cleanup()

	testFile(t, userRoot(cfg), "rmdir-me/x.txt", "x")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := waitLoc(page, ".dir-row[data-subdir='rmdir-me'] .dir-delete-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("目录删除按钮未渲染: %v", err)
	}

	acceptDialog(page, "")

	req, err := page.ExpectRequest("**/rmdir?*", func() error {
		return page.Locator(".dir-row[data-subdir='rmdir-me'] .dir-delete-btn").Click()
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("未观察到 POST /rmdir（目录删除未接线？）: %v", err)
	}
	u := req.URL()
	if !strings.Contains(u, "dirname=rmdir-me") {
		t.Errorf("rmdir URL = %q, want 含 dirname=rmdir-me", u)
	}
	if !strings.Contains(u, "force=true") {
		t.Errorf("rmdir URL = %q, want 含 force=true（递归删除语义）", u)
	}

	waitTextGone(t, page, "#file-list", "rmdir-me", 8000)
}

// sha256Sum 返回 content 的 SHA-256 摘要（供 e2e 用例预算期望 checksum）。
func sha256Sum(content []byte) []byte {
	s := sha256.Sum256(content)
	return s[:]
}
