// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// trash_view_e2e_test.go Playwright e2e：回收站视图。
// 上传文件 → 软删 → 点击回收站按钮 → 断言面板含原路径。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestTrashView_Panel 回收站面板渲染（软删后可见条目）。
func TestTrashView_Panel(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	// 上传文件。
	if st, _ := seedUploadMultipart(t, baseURL, "", "trash-view.txt", []byte("x")); st != 200 {
		t.Fatalf("seed upload = %d", st)
	}
	// 软删（POST /delete?soft=true + checksum）。
	delReq, _ := http.NewRequest("POST", baseURL+"/delete?filename=trash-view.txt&soft=true", nil)
	delReq.Header.Set("X-File-Checksum", sha256HexBytes([]byte("x")))
	delResp, derr := http.DefaultClient.Do(delReq)
	if derr != nil {
		t.Fatalf("soft delete: %v", derr)
	}
	delResp.Body.Close()
	if delResp.StatusCode != 200 {
		t.Fatalf("soft delete = %d", delResp.StatusCode)
	}

	// 点击回收站按钮。
	if err := page.Locator("#trash-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click trash: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	html, err := page.Locator("#trash-panel").InnerHTML()
	if err != nil {
		t.Fatalf("trash panel: %v", err)
	}
	if !strings.Contains(html, "trash-view.txt") {
		t.Fatalf("trash 面板应含原路径: %s", clipStr(html, 300))
	}
}

// TestTrashView_MultiVolume_VolumeColumnFilterAndEmpty 回收站**多卷 UI**（用户裁定 2026-10-19 ③：
// 各卷独立管理 + 汇总展示）：两个本地卷各软删一个文件 ⇒ 汇总视图显示**卷列**（两条、卷名不同）、
// 卷筛选可切到只显示该卷、「清空全部卷」二次确认后清空（面板变「回收站为空」）。
//
// 后端多卷语义另由 pkg/server/trash_multivolume_test.go 端到端锁定；本用例只管 UI 渲染与交互。
func TestTrashView_MultiVolume_VolumeColumnFilterAndEmpty(t *testing.T) {
	t.Parallel()
	dir1, dir2 := t.TempDir(), t.TempDir()
	baseURL, _, cleanup := testServerCfg(t, func(cfg *server.Config) {
		cfg.StorageRoot = dir1
		cfg.Volumes = []server.VolumeConfig{
			{Name: "default", Root: dir1},
			{Name: "v2", Root: dir2},
		}
	})
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()
	// 清空是破坏性动作，前端有二次确认（confirm）⇒ 自动接受。
	page.On("dialog", func(d playwright.Dialog) { _ = d.Accept() })

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	// 两卷各一文件 + 各软删（软删落**文件所在卷**的 trash）。
	bodyA, bodyB := []byte("mv-a"), []byte("mv-b")
	if st, _ := seedUploadMultipart(t, baseURL, "", "mv-a.txt", bodyA); st != 200 {
		t.Fatalf("seed upload default = %d", st)
	}
	if st, _ := seedUploadMultipart(t, baseURL, "v2", "mv-b.txt", bodyB); st != 200 {
		t.Fatalf("seed upload v2 = %d", st)
	}
	softDeleteInVolume(t, baseURL, "mv-a.txt", "", bodyA)
	softDeleteInVolume(t, baseURL, "mv-b.txt", "v2", bodyB)

	if err := page.Locator("#trash-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click trash: %v", err)
	}
	// 汇总面板：两条 + 两卷名 + 控件齐备。
	panelHTML := waitPanelContains(t, page, []string{"mv-a.txt", "mv-b.txt", "default", "v2"})
	for _, id := range []string{"trash-volume-filter", "trash-empty-vol-btn", "trash-empty-all-btn"} {
		if !strings.Contains(panelHTML, id) {
			t.Fatalf("回收站面板应含控件 %s: %s", id, clipStr(panelHTML, 400))
		}
	}

	// 卷筛选切到 v2 ⇒ 只显示 v2 的条目。
	v2 := []string{"v2"}
	if _, err := page.Locator("#trash-volume-filter").SelectOption(playwright.SelectOptionValues{Values: &v2}); err != nil {
		t.Fatalf("select volume filter: %v", err)
	}
	filtered := waitPanelContains(t, page, []string{"mv-b.txt"})
	if strings.Contains(filtered, "mv-a.txt") {
		t.Fatalf("筛选 v2 后不应显示默认卷条目: %s", clipStr(filtered, 400))
	}

	// 清空全部卷（确认已自动接受）⇒ 面板变「回收站为空」。
	if err := page.Locator("#trash-empty-all-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click empty all: %v", err)
	}
	waitPanelContains(t, page, []string{"回收站为空"})
}

// softDeleteInVolume 软删（vol 为空 ⇒ 默认卷），带校验和头。
func softDeleteInVolume(t *testing.T, baseURL, filename, vol string, body []byte) {
	t.Helper()
	target := baseURL + "/delete?filename=" + filename + "&soft=true"
	if vol != "" {
		target += "&volume=" + vol
	}
	req, _ := http.NewRequest("POST", target, nil)
	req.Header.Set("X-File-Checksum", sha256HexBytes(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("soft delete %s: %v", filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("soft delete %s = %d", filename, resp.StatusCode)
	}
}

// waitPanelContains 等待 `#trash-panel` 的 HTML 含全部子串（用 Playwright **条件等待**，不用固定
// sleep：sleep 棘轮只减不增，且条件等待本身更稳），返回最终 HTML；超时则失败并附当前 HTML。
func waitPanelContains(t *testing.T, page playwright.Page, wants []string) string {
	t.Helper()
	conds := make([]string, 0, len(wants))
	for _, w := range wants {
		conds = append(conds, "el.innerHTML.indexOf("+jsQuote(w)+") >= 0")
	}
	expr := "() => { const el = document.getElementById('trash-panel'); return !!el && " +
		strings.Join(conds, " && ") + "; }"
	if _, err := page.WaitForFunction(expr, nil, playwright.PageWaitForFunctionOptions{
		Timeout: playwright.Float(8000),
	}); err != nil {
		html, _ := page.Locator("#trash-panel").InnerHTML()
		t.Fatalf("回收站面板未在预期内包含 %v；当前: %s（%v）", wants, clipStr(html, 400), err)
	}
	html, herr := page.Locator("#trash-panel").InnerHTML()
	if herr != nil {
		t.Fatalf("读回收站面板: %v", herr)
	}
	return html
}

// jsQuote 把字符串转为 JS 单引号字面量（转义反斜杠与单引号；用例内的期望子串均无引号，此处仅防御）。
func jsQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "'", `\'`)
	return "'" + s + "'"
}

// sha256HexBytes 返回 SHA-256 hex（delete checksum）。
func sha256HexBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
