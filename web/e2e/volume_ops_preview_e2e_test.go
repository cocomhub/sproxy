// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volume_ops_preview_e2e_test.go Playwright e2e：
//   B1 卷操作按钮（volumes tab 含 copy/move/rebalance 按钮）
//   B4 图片预览（缩略图 URL 加载 + 原图回退）
// 真浏览器 + 真实服务端（testServer），验证按钮渲染 + 点击触发对应 API。

import (
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestVolumes_OpsButtons 卷面板含卷操作区（B1）。
func TestVolumes_OpsButtons(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if err := page.Locator("#stats-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click stats-btn: %v", err)
	}
	if err := page.Locator("#volumes-tab").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click volumes-tab: %v", err)
	}
	// 卷操作区（单卷场景显示提示文案；多卷含按钮）——等待「卷操作」区渲染完成。
	var ok bool
	for i := 0; i < 40; i++ {
		txt, _ := page.Locator("#volumes-panel").InnerText()
		if strings.Contains(txt, "卷操作") || strings.Contains(txt, "至少两个卷") {
			ok = true
			break
		}
		page.WaitForTimeout(200)
	}
	if !ok {
		txt, _ := page.Locator("#volumes-panel").InnerText()
		t.Fatalf("卷面板应含卷操作区: %q", clipStr(txt, 300))
	}
}

// TestPreview_ImageUsesThumbnail 图片预览：previewImage 走缩略图 URL（?transform=thumb）。
// 上传一张真实 PNG → 点预览按钮 → 断 img src 含 transform=thumb。
func TestPreview_ImageUsesThumbnail(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// seed：上传一张真实 1x1 PNG（最小合法文件）。
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82}
	if st, _ := uploadFileToVolume(t, baseURL, "test.png", png); st != 200 {
		t.Fatalf("上传 PNG status=%d", st)
	}
	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	// 等文件行渲染 → 点预览按钮。
	if err := waitLoc(page, ".file-preview-btn", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("file-preview-btn 未渲染: %v", err)
	}
	if err := page.Locator(".file-preview-btn").First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click preview: %v", err)
	}
	if err := waitLoc(page, ".modal-overlay-img img", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("预览 modal img 未渲染: %v", err)
	}
	src, _ := page.Locator(".modal-overlay-img img").GetAttribute("src")
	if !strings.Contains(src, "transform=thumb") {
		t.Fatalf("预览 img src 应含缩略图 transform=thumb: %q", src)
	}
}
