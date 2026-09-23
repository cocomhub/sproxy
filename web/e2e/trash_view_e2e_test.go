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

// sha256HexBytes 返回 SHA-256 hex（delete checksum）。
func sha256HexBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
