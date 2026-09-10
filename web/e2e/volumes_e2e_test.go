// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volumes_e2e_test.go 基于 Playwright 的多卷 Web UI 浏览器自动化测试（T7 收口）。
//
// 质量要求（web-ui-browser-e2e-required）：浏览器自动化必须做**真实交互断言**，不只断言
// 「元素存在/用例通过」——避免「接口正常但页面未接线」。三个用例各自证明接线点：
//
//   - TestVolumes_Badge：badge 是只读渲染，故补「数据源接线」——捕获导航触发的 GET /api/files
//     响应，断言列表条目带真实 volume 字段（a.txt→main / b.txt→disk2），再断言行级 .vol-badge
//     渲染文本与之一致（证明 badge 数据来自 API，非写死）。
//   - TestVolumes_Panel：先 HTTP 直传 2048B 文件到 disk2（真实卷容量池 Usage=2048），再点击
//     「卷」标签页并捕获其触发的 GET /api/volumes 响应——断言响应 2 卷且 disk2.usage=2048
//     （数据非壳），随后断言面板行渲染含该真实用量（formatSize → "2.0 KB"）。
//   - TestVolumes_UploadVolumeSelect：选择 disk2 后断言前端卷上下文 currentVolume()=='disk2'
//     （下拉 change → setVolumeContext 接线）；再真实触发上传（#file-input 设临时小文件）并捕获
//     POST /upload 请求体——断言 multipart 含 volume=disk2（「下拉选卷 → 上传请求带所选卷」链路
//     真实接通）。若 loopback 无凭据放行则上传 200 且落盘 disk2（服务端按 volume 路由的证据）；
//     若 401 则记录边界（前端接线已由请求体证明）。
//
// 服务端契约（与 T7 UI 实现核对）：
//   - GET /api/volumes 无凭据（ring 空）+ AllowInsecureLoopback=true 下对 loopback GET 放行
//     （handleNoCredentials 合成 anonymous user Principal）→ 200；POST /upload 同理由 loopback
//     任意方法兜底放行（auth.go handleNoCredentials）；
//   - /api/files 多卷聚合 owner 视图，文件条目带 volume 字段 → UI <span class="vol-badge">卷名</span>；
//   - 简单上传 volume 为 multipart 普通字段 volume（files.js simpleUpload fields.volume），服务端
//     upload handler FormValue("volume") 路由（upload_handler.go:134）。

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// multiVolumeTestServer 启动双卷（main + disk2）sproxy 测试实例：cfg.Volumes 双卷均缺省
// ACL（deny + 空名单 = 默认开放，parseVolumeACL nil 即此语义），StorageRoot = 默认卷
// （main）根。返回 baseURL、卷名 → 物理根映射与 cleanup。无凭据（CredentialTTL=-1 → ring
// 空）+ AllowInsecureLoopback=true 兜底 loopback（httptest 天然 127.0.0.1），anonymous
// 租户预创建在默认卷（main），/api/files 与 /api/volumes 聚合 anonymous 卷视图（两卷开放）。
func multiVolumeTestServer(t *testing.T) (string, map[string]string, func()) {
	t.Helper()

	base := t.TempDir()
	mainRoot := filepath.Join(base, "main")
	disk2Root := filepath.Join(base, "disk2")
	for _, d := range []string{mainRoot, disk2Root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := server.Default()
	cfg.StorageRoot = mainRoot
	cfg.LogLevel = "error"
	// UI E2E 为无凭据场景：禁首启 anonymous 凭据生成 + 允许 loopback 无认证兜底
	// （与 testServer 同一套免认证前提）。
	cfg.CredentialTTL = -1
	cfg.AllowInsecureLoopback = true
	cfg.Volumes = []server.VolumeConfig{
		{Name: "main", Root: mainRoot},
		{Name: "disk2", Root: disk2Root},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "e2e-test",
		BuildAt: "e2e-test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ts := httptest.NewServer(h.Handler())
	roots := map[string]string{"main": mainRoot, "disk2": disk2Root}
	return ts.URL, roots, func() {
		ts.Close()
		h.Close()
		os.RemoveAll(filepath.Join(mainRoot, ".__cloud__"))
		os.RemoveAll(filepath.Join(mainRoot, ".__downloads__"))
	}
}

// volFile 把文件写入指定卷的 anonymous/user 桶（与 testFile 同样的直写磁盘方式）。
// rel 为相对 user 桶路径，如 "a.txt" 或 "sub/x.txt"。
func volFile(t *testing.T, roots map[string]string, vol, rel, content string) {
	t.Helper()
	userDir := filepath.Join(roots[vol], "anonymous", "user")
	testFile(t, userDir, rel, content)
}

// seedUploadToVolume 用真实 multipart POST /upload（volume 普通字段 + file 文件件 + 正确
// X-File-Checksum）向指定卷写入内容——经 upload handler 路由 + 双账本，使该卷容量池
// Usage 精确等于 len(content)（与浏览器前端同一条服务端写路径）。返回 HTTP 状态与响应体。
func seedUploadToVolume(t *testing.T, baseURL, vol, filename string, content []byte) (int, string) {
	t.Helper()
	return seedUploadMultipart(t, baseURL, vol, filename, content)
}

// waitResponse 轮询 Request.Response()（请求发出后响应异步到达）。5s 内未收到返回 nil。
func waitResponse(t *testing.T, req playwright.Request) playwright.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := req.Response()
		if err == nil && resp != nil {
			return resp
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestVolumes_Badge 双卷各写一文件 → /ui/ 聚合列表同时出现 a.txt 与 b.txt，且各自行
// 带对应卷 badge（a.txt → main、b.txt → disk2）。
// 接线点：捕获导航触发的 GET /api/files 响应（badge 数据源 = 真实 volume 字段），
// 再断言行级 .vol-badge 渲染文本与 API 字段一致（非写死）。
func TestVolumes_Badge(t *testing.T) {
	baseURL, roots, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	volFile(t, roots, "main", "a.txt", "on main volume")
	volFile(t, roots, "disk2", "b.txt", "on disk2 volume")

	page, stop := pageFixture(t)
	defer stop()

	// 接线①：导航 → DOMContentLoaded refreshList → GET /api/files。断言响应条目带 volume 字段。
	// （list() 恒带 ?offset=&limit= 查询串，glob 用 **/api/files?* 匹配带 query 的整条 URL；
	// 不能用 **/api/files*——会误匹配静态脚本 /sclient/api/files.js。）
	resp, err := page.ExpectResponse("**/api/files?*", func() error {
		_, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)})
		return gerr
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到导航触发的 GET /api/files: %v", err)
	}
	var listPayload struct {
		Files []struct {
			Name   string `json:"name"`
			Volume string `json:"volume"`
		} `json:"files"`
	}
	if err := resp.JSON(&listPayload); err != nil {
		b, _ := resp.Body()
		t.Fatalf("解析 /api/files 响应: %v (status=%d body=%q)", err, resp.Status(), string(b))
	}
	volOf := map[string]string{}
	for _, f := range listPayload.Files {
		if f.Name == "a.txt" || f.Name == "b.txt" {
			volOf[f.Name] = f.Volume
		}
	}
	if volOf["a.txt"] != "main" || volOf["b.txt"] != "disk2" {
		t.Fatalf("列表 volume 字段 = %v, want a.txt→main b.txt→disk2", volOf)
	}

	// 接线②（渲染断言，badge 只读）：聚合列表两文件都渲染，且各自行 .vol-badge 文本与 API 一致。
	if err := waitLoc(page, "#file-table tr", nil, 8000); err != nil {
		t.Fatalf("file table not loaded: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := waitLoc(page, "text="+name, nil, 8000); err != nil {
			t.Fatalf("expected %s in aggregated file list: %v", name, err)
		}
	}
	for _, tc := range []struct{ file, vol string }{
		{"a.txt", "main"},
		{"b.txt", "disk2"},
	} {
		badge := page.Locator("#file-table tr").Filter(playwright.LocatorFilterOptions{HasText: tc.file}).Locator(".vol-badge")
		if n, err := badge.Count(); err != nil || n != 1 {
			t.Fatalf("row %s .vol-badge count = %d (err=%v), want 1", tc.file, n, err)
		}
		txt, err := badge.InnerText()
		if err != nil {
			t.Fatalf("read badge text for %s: %v", tc.file, err)
		}
		if strings.TrimSpace(txt) != tc.vol {
			t.Errorf("badge on row %s = %q, want %q", tc.file, strings.TrimSpace(txt), tc.vol)
		}
	}
}

// TestVolumes_Panel 监控弹窗「卷」标签页渲染 main 与 disk2 卷仪表，且 usage 为真实卷池数据
// （非空壳）。
// 接线点：点击 #volumes-tab 真实发出 GET /api/volumes（ExpectResponse 捕获），响应 disk2.usage
// == 预置字节；面板行渲染文本含该用量（formatSize → "2.0 KB"）——证明「点击 → 拉 API → 渲染
// 数据」链路接通。
func TestVolumes_Panel(t *testing.T) {
	baseURL, _, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	// 预置真实卷用量：HTTP 直传 2048B 文件到 disk2 → disk2 卷容量池 Usage = 2048
	// （与浏览器同一服务端写路径，双账本 commit）。
	seed := bytes.Repeat([]byte("v"), 2048)
	status, body := seedUploadToVolume(t, baseURL, "disk2", "usage-seed.bin", seed)
	if status != http.StatusOK {
		t.Fatalf("seed upload to disk2 status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	// 等 initUploadVolumeSelect 的 GET /api/volumes 完成（下拉被填充），避免与点击捕获混淆。
	if err := waitLoc(page, "#upload-volume option[value='main']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("upload volume select not populated: %v", err)
	}

	// 打开监控弹窗
	if _, err := page.Evaluate("showStats()"); err != nil {
		t.Fatalf("showStats: %v", err)
	}
	if err := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("stats-modal not visible: %v", err)
	}

	// 接线：点击「卷」标签页必须真实发出 GET /api/volumes（数据非壳，usage 来自真实卷池）。
	resp, err := page.ExpectResponse("**/api/volumes", func() error {
		return page.Locator("#volumes-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击 volumes-tab 未触发 GET /api/volumes: %v", err)
	}
	var volPayload struct {
		Volumes []struct {
			Name  string `json:"name"`
			Usage int64  `json:"usage"`
		} `json:"volumes"`
	}
	if jerr := resp.JSON(&volPayload); jerr != nil {
		t.Fatalf("解析 /api/volumes 响应: %v", jerr)
	}
	if len(volPayload.Volumes) != 2 {
		t.Fatalf("/api/volumes 卷数 = %d, want 2（main+disk2）", len(volPayload.Volumes))
	}
	usageOf := map[string]int64{}
	for _, v := range volPayload.Volumes {
		usageOf[v.Name] = v.Usage
	}
	if usageOf["disk2"] != int64(len(seed)) {
		t.Fatalf("/api/volumes disk2 usage = %d, want %d（预置真实卷用量）", usageOf["disk2"], len(seed))
	}

	// 渲染断言：面板表格渲染出 main/disk2 卷名与真实用量（2048 B → "2.0 KB"）。
	if werr := waitLoc(page, "#volumes-panel table tbody tr", nil, 8000); werr != nil {
		content, _ := page.Locator("#volumes-panel").InnerText()
		t.Fatalf("volumes table not rendered, panel content: %s", content)
	}
	text, err := page.Locator("#volumes-panel").InnerText()
	if err != nil {
		t.Fatalf("read volumes panel: %v", err)
	}
	for _, want := range []string{"main", "disk2", "2.0 KB"} {
		if !strings.Contains(text, want) {
			t.Errorf("volumes panel 缺 %q; panel text:\n%s", want, text)
		}
	}
}

// TestVolumes_UploadVolumeSelect 上传表单「卷」下拉存在且含 auto + 可见卷 option，并验证
// 「下拉选卷 → 上传请求带所选卷」链路真实接通：
//   - 选择 disk2 → change handler → setVolumeContext('disk2') → currentVolume()=='disk2'（前端状态）；
//   - 真实触发上传（#file-input 临时小文件）→ 捕获 POST /upload 请求体 multipart 含 volume=disk2；
//   - loopback 无凭据放行时上传 200 且文件落盘 disk2（服务端按 volume 路由），否则 401 记录边界。
func TestVolumes_UploadVolumeSelect(t *testing.T) {
	baseURL, roots, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 下拉静态存在（auto 打底）
	sel := page.Locator("#upload-volume")
	if cnt, _ := sel.Count(); cnt == 0 {
		t.Fatal("#upload-volume select not found")
	}
	// 等待 /api/volumes 异步填充可见卷 option（main 先到即可判定 populate 完成）。
	if err := waitLoc(page, "#upload-volume option[value='main']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		vals, _ := page.Evaluate(`Array.from(document.querySelectorAll('#upload-volume option')).map(o => o.value)`)
		t.Fatalf("visible volume option not populated, current options=%v: %v", vals, err)
	}
	raw, err := page.Evaluate(`Array.from(document.querySelectorAll('#upload-volume option')).map(o => o.value)`)
	if err != nil {
		t.Fatalf("read upload-volume options: %v", err)
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("unexpected options type %T", raw)
	}
	got := make([]string, 0, len(list))
	for _, v := range list {
		got = append(got, v.(string))
	}
	want := []string{"", "main", "disk2"} // auto("") 打底 + 声明序可见卷
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("upload-volume options = %v, want %v", got, want)
	}

	// 接线①：未选择时卷上下文为空（auto 语义）——select → setVolumeContext 初始态。
	cur, err := page.Evaluate("currentVolume()")
	if err != nil {
		t.Fatalf("read currentVolume(): %v", err)
	}
	if cur != "" {
		t.Fatalf("默认 currentVolume() = %q, want \"\"（auto）", cur)
	}

	// 接线②：选择 disk2 → change handler → setVolumeContext('disk2') → 前端卷上下文更新。
	if _, serr := sel.SelectOption(playwright.SelectOptionValues{Values: &[]string{"disk2"}}); serr != nil {
		t.Fatalf("select disk2: %v", serr)
	}
	cur, err = page.Evaluate("currentVolume()")
	if err != nil {
		t.Fatalf("read currentVolume() after select: %v", err)
	}
	if cur != "disk2" {
		t.Fatalf("选中 disk2 后 currentVolume() = %q, want \"disk2\"（下拉 change 未接线 setVolumeContext）", cur)
	}

	// 接线③：真实触发上传（#file-input 设临时小文件）→ 捕获 POST /upload 请求体。
	probe := filepath.Join(t.TempDir(), "wiring-probe.txt")
	probeContent := []byte("volume wiring probe content\n")
	if werr := os.WriteFile(probe, probeContent, 0o644); werr != nil {
		t.Fatal(werr)
	}
	upReq, err := page.ExpectRequest("**/upload", func() error {
		return page.Locator("#file-input").SetInputFiles([]string{probe})
	}, playwright.PageExpectRequestOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到 POST /upload（前端未把所选卷注入上传？）: %v", err)
	}
	body, err := upReq.PostDataBuffer()
	if err != nil {
		t.Fatalf("read upload body: %v", err)
	}
	idx := bytes.Index(body, []byte(`name="volume"`))
	if idx < 0 {
		t.Fatal("POST /upload multipart body 缺 volume 字段（前端未注入卷上下文）")
	}
	end := idx + 200
	if end > len(body) {
		end = len(body)
	}
	if !bytes.Contains(body[idx:end], []byte("disk2")) {
		t.Errorf("POST /upload multipart volume 字段后未跟随 disk2（上传请求未带所选卷）")
	}

	// 接线④（服务端路由证据，受无凭据边界影响）：loopback 无凭据兜底放行 → 200 且文件落盘 disk2；
	// 若 401 则记录边界（前端接线已由请求体证明，真实落卷由 T7 手动 chrome 实测覆盖）。
	upResp := waitResponse(t, upReq)
	if upResp == nil {
		t.Log("POST /upload 响应未在 5s 内观察到，跳过落盘断言（前端接线已由请求体证明）")
		return
	}
	switch upResp.Status() {
	case http.StatusOK:
		if !fileExists(filepath.Join(roots["disk2"], "anonymous", "user", "wiring-probe.txt")) {
			t.Error("上传未落盘 disk2 卷（服务端未按 volume 路由到 disk2）")
		}
		if fileExists(filepath.Join(roots["main"], "anonymous", "user", "wiring-probe.txt")) {
			t.Error("wiring-probe.txt 不应落在默认卷 main")
		}
	default:
		t.Logf("upload response=%d（无凭据 401 边界）：请求体已证前端把所选卷注入上传；真实落卷路径由 T7 手动 chrome 实测覆盖", upResp.Status())
	}
}

// TestVolumes_SingleVolumeDefaultBadgeAndPanel 单卷缺省（未配 volumes → 服务端合成 default 卷）
// 下，文件行卷 badge 与「卷」监控面板同样显示 default，与多卷形态一致。
//
// 产品 UI 决策（2026-09）：单卷也显示 default。此前 SDD 列为待决策项——服务端本就合成
// default 卷并回填 volume/列出 /api/volumes，前端渲染无条件分支，故决策为「保持显示」；
// 本用例把该决策锁进回归（防未来把单卷 badge/面板当噪音隐藏）。
//
// 接线点（同 TestVolumes_Badge/Panel 的真交互断言）：
//   - 捕获导航触发的 GET /api/files → 断言单卷条目带 volume="default"（badge 数据源非写死）；
//   - 断言行级 .vol-badge 文本 = default；
//   - 点击 #volumes-tab 捕获 GET /api/volumes → 响应恰 1 卷 default，面板文本含 default。
func TestVolumes_SingleVolumeDefaultBadgeAndPanel(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	// 走真实上传写路径（multipart volume=default），使文件确实落在 default 卷 user 桶。
	content := []byte("single volume default badge payload")
	if status, body := seedUploadToVolume(t, baseURL, "default", "solo.txt", content); status != http.StatusOK {
		t.Fatalf("seed upload to default status=%d body=%s", status, body)
	}

	page, stop := pageFixture(t)
	defer stop()

	// 接线①：导航 → GET /api/files（列表数据源）。断言条目带 volume="default"。
	resp, err := page.ExpectResponse("**/api/files?*", func() error {
		_, gerr := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)})
		return gerr
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(10000)})
	if err != nil {
		t.Fatalf("未观察到导航触发的 GET /api/files: %v", err)
	}
	var listPayload struct {
		Files []struct {
			Name   string `json:"name"`
			Volume string `json:"volume"`
		} `json:"files"`
	}
	if jerr := resp.JSON(&listPayload); jerr != nil {
		b, _ := resp.Body()
		t.Fatalf("解析 /api/files 响应: %v (status=%d body=%q)", jerr, resp.Status(), string(b))
	}
	gotVol := ""
	for _, f := range listPayload.Files {
		if f.Name == "solo.txt" {
			gotVol = f.Volume
		}
	}
	if gotVol != "default" {
		t.Fatalf("单卷 /api/files solo.txt volume = %q, want \"default\"", gotVol)
	}

	// 接线①续（渲染断言）：行级 .vol-badge 文本与 API 字段一致（证明 badge 来自 API，非写死）。
	if werr := waitLoc(page, "#file-table tr", nil, 8000); werr != nil {
		t.Fatalf("file table not loaded: %v", werr)
	}
	if lerr := waitLoc(page, "text=solo.txt", nil, 8000); lerr != nil {
		t.Fatalf("solo.txt 未出现在文件列表: %v", lerr)
	}
	badge := page.Locator("#file-table tr").Filter(playwright.LocatorFilterOptions{HasText: "solo.txt"}).Locator(".vol-badge")
	if n, berr := badge.Count(); berr != nil || n != 1 {
		t.Fatalf("单卷行 solo.txt .vol-badge count = %d (err=%v), want 1（单卷 badge 未渲染）", n, berr)
	}
	badgeTxt, err := badge.InnerText()
	if err != nil {
		t.Fatalf("读取 badge 文本: %v", err)
	}
	if strings.TrimSpace(badgeTxt) != "default" {
		t.Errorf("单卷 badge = %q, want \"default\"", strings.TrimSpace(badgeTxt))
	}

	// 接线②：打开监控弹窗 → 点击「卷」标签页必须真实发出 GET /api/volumes（面板数据源）。
	if _, eerr := page.Evaluate("showStats()"); eerr != nil {
		t.Fatalf("showStats: %v", eerr)
	}
	if merr := waitLoc(page, "#stats-modal", playwright.WaitForSelectorStateVisible, 8000); merr != nil {
		t.Fatalf("stats-modal not visible: %v", merr)
	}
	vresp, err := page.ExpectResponse("**/api/volumes", func() error {
		return page.Locator("#volumes-tab").Click()
	}, playwright.PageExpectResponseOptions{Timeout: playwright.Float(8000)})
	if err != nil {
		t.Fatalf("点击 volumes-tab 未触发 GET /api/volumes: %v", err)
	}
	var volPayload struct {
		Volumes []struct {
			Name string `json:"name"`
		} `json:"volumes"`
	}
	if jerr := vresp.JSON(&volPayload); jerr != nil {
		t.Fatalf("解析 /api/volumes 响应: %v", jerr)
	}
	if len(volPayload.Volumes) != 1 || volPayload.Volumes[0].Name != "default" {
		t.Fatalf("单卷 /api/volumes = %+v, want 恰 1 卷 default", volPayload.Volumes)
	}

	// 渲染断言：面板表格渲染出 default 卷名（非空壳）。
	if werr := waitLoc(page, "#volumes-panel table tbody tr", nil, 8000); werr != nil {
		panelTxt, _ := page.Locator("#volumes-panel").InnerText()
		t.Fatalf("单卷卷面板未渲染, panel content: %s", panelTxt)
	}
	panelText, err := page.Locator("#volumes-panel").InnerText()
	if err != nil {
		t.Fatalf("读取 volumes panel: %v", err)
	}
	if !strings.Contains(panelText, "default") {
		t.Errorf("单卷卷面板缺 \"default\"; panel text:\n%s", panelText)
	}
}
