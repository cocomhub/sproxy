// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volumes_e2e_test.go 基于 Playwright 的多卷 Web UI 浏览器自动化测试（T7 收口）：
//   - TestVolumes_Badge：双卷各一文件 → 聚合列表同时显示两文件，且文件行带正确卷 badge；
//   - TestVolumes_Panel：监控弹窗「卷」标签页渲染每卷容量/用量仪表（GET /api/volumes）；
//   - TestVolumes_UploadVolumeSelect：上传表单「卷」下拉含 auto + 可见卷 option。
//
// 服务端契约（与 T7 UI 实现核对）：
//   - GET /api/volumes 在无凭据（ring 空）+ AllowInsecureLoopback=true 下对 loopback 来源
//     GET 放行（handleNoCredentials 合成 anonymous user Principal），返回 {volumes:[…]}；
//   - 文件列表（GET /api/files）多卷聚合 owner 视图，文件条目带 volume 字段 → UI 渲染
//     <span class="vol-badge">卷名</span>；
//   - 上传「卷」下拉 <select id="upload-volume"> 首 option 为 auto("")，
//     populateUploadVolumeSelect 按 /api/volumes 顺序追加可见卷 option。

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

// TestVolumes_Badge 双卷各写一文件 → /ui/ 聚合列表同时出现 a.txt 与 b.txt，且各自行
// 带对应卷 badge（a.txt → main、b.txt → disk2）。断言用实际 DOM：行文本 + .vol-badge。
func TestVolumes_Badge(t *testing.T) {
	baseURL, roots, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	volFile(t, roots, "main", "a.txt", "on main volume")
	volFile(t, roots, "disk2", "b.txt", "on disk2 volume")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	if _, err := page.WaitForSelector("#file-table tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("file table not loaded: %v", err)
	}
	// 两文件异步渲染后都出现在聚合列表（分页首屏含全部两文件）。
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := page.WaitForSelector("text="+name, playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(8000)}); err != nil {
			t.Fatalf("expected %s in aggregated file list: %v", name, err)
		}
	}

	// 行级断言：a.txt 所在行的 badge = main，b.txt 所在行 badge = disk2。
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

// TestVolumes_Panel 监控弹窗「卷」标签页渲染 main 与 disk2 卷仪表。
// /api/volumes 在 AllowInsecureLoopback（ring 空 + loopback GET）下放行 → 渲染真实卷表；
// 若服务端收紧为 401，本用例会断言卷名渲染失败而暴露契约漂移（T7 需保持该降级路径）。
func TestVolumes_Panel(t *testing.T) {
	baseURL, _, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// 打开监控弹窗
	if _, err := page.Evaluate("showStats()"); err != nil {
		t.Fatalf("showStats: %v", err)
	}
	if _, err := page.WaitForSelector("#stats-modal", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(8000),
	}); err != nil {
		t.Fatalf("stats-modal not visible: %v", err)
	}

	// 切到「卷」标签页（showVolumes 异步拉 /api/volumes 渲染 #volumes-panel）
	if err := page.Locator("#volumes-tab").Click(); err != nil {
		t.Fatalf("click volumes-tab: %v", err)
	}
	if _, err := page.WaitForSelector("#volumes-panel table tbody tr", playwright.PageWaitForSelectorOptions{Timeout: playwright.Float(8000)}); err != nil {
		content, _ := page.Locator("#volumes-panel").InnerText()
		t.Fatalf("volumes table not rendered, panel content: %s", content)
	}

	text, err := page.Locator("#volumes-panel").InnerText()
	if err != nil {
		t.Fatalf("read volumes panel: %v", err)
	}
	for _, vol := range []string{"main", "disk2"} {
		if !strings.Contains(text, vol) {
			t.Errorf("volumes panel missing volume %q; panel text:\n%s", vol, text)
		}
	}
}

// TestVolumes_UploadVolumeSelect 上传表单「卷」下拉存在且含 auto + 可见卷（main/disk2）
// option。populateUploadVolumeSelect 由 /api/volumes GET 填充；无凭据下该端点 loopback
// 放行（否则下拉保持 auto 单 option 降级——本用例验证的是放行渲染路径）。
func TestVolumes_UploadVolumeSelect(t *testing.T) {
	baseURL, _, cleanup := multiVolumeTestServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")

	// select 静态存在（auto 打底）
	if cnt, _ := page.Locator("#upload-volume").Count(); cnt == 0 {
		t.Fatal("#upload-volume select not found")
	}
	// 等待 /api/volumes 异步填充可见卷 option（main 先到即可判定 populate 完成）。
	// option 在收起的下拉内不属于 visible 元素 → State=Attached（DOM 挂载即成功）。
	if _, err := page.WaitForSelector("#upload-volume option[value='main']", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(8000),
	}); err != nil {
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
}
