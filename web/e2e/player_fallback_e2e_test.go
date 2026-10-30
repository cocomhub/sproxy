// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// player_fallback_e2e_test.go — 不可播格式的「本地播放器兜底」真浏览器 E2E（PR #894）。
//
// 断言口径（三要素：控件操作 → 网络/DOM 变化 → 反向不成立）：
//   - AVI（浏览器不支持）点「▶ 播放」→ **不渲染 <video>**，而是渲染 .video-modal-unsupported，
//     且含「用本地播放器打开」/「复制直链」/「下载原文件」三个兜底动作；
//   - 「复制直链」写入的 URL 形如 /download?filename=...（有凭据时附 ?spx_sig=）；
//   - MP4（浏览器可播）点「▶ 播放」→ 渲染 <video>，且**不**出现兜底弹层。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mxschmitt/playwright-go"
)

// TestPlayerFallback_UnsupportedFormat 浏览器不可播格式走本地播放器兜底（不再黑屏）。
func TestPlayerFallback_UnsupportedFormat(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := testServer(t)
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()

	// 记录剪贴板写入（无凭据环境避免依赖真实 clipboard 权限）。
	if err := page.AddInitScript(playwright.Script{Content: playwright.String(`
		window.__copied = [];
		if (navigator.clipboard) {
			navigator.clipboard.writeText = function (s) {
				window.__copied.push(String(s));
				return Promise.resolve();
			};
		}
	`)}); err != nil {
		t.Fatalf("注入剪贴板桩: %v", err)
	}
	if _, err := page.Goto(baseURL + "/ui/"); err != nil {
		t.Fatalf("打开 UI: %v", err)
	}

	dir := t.TempDir()
	avi := filepath.Join(dir, "demo.avi")
	mp4 := filepath.Join(dir, "demo.mp4")
	if err := os.WriteFile(avi, []byte("RIFF....AVI LIST fake-avi-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp4, []byte("\x00\x00\x00\x18ftypisom fake-mp4-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{avi, mp4} {
		if err := page.Locator("#file-input").SetInputFiles(p); err != nil {
			t.Fatalf("上传 %s: %v", filepath.Base(p), err)
		}
		if werr := waitLoc(page, "text="+filepath.Base(p), nil, 15000); werr != nil {
			t.Fatalf("列表未出现 %s: %v", filepath.Base(p), werr)
		}
	}

	// ---- AVI：兜底弹层（无 <video>）----
	if err := page.Locator(`tr:has-text("demo.avi") .file-video-play-btn`).First().Click(); err != nil {
		t.Fatalf("点击 avi 播放: %v", err)
	}
	if werr := waitLoc(page, ".video-modal-unsupported", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf("不可播格式应出现兜底弹层: %v", werr)
	}
	if n, _ := page.Locator(".video-modal video").Count(); n != 0 {
		t.Fatalf("不可播格式不应渲染 <video>（会黑屏），got %d", n)
	}
	for _, sel := range []string{"[data-open-local-player]", "[data-copy-stream-url]", "a[download]"} {
		if n, _ := page.Locator(".video-modal-unsupported " + sel).Count(); n == 0 {
			t.Fatalf("兜底弹层缺少 %s", sel)
		}
	}

	// ---- 复制直链：URL 为 /download?filename=...（有凭据时附 spx_sig）----
	if err := page.Locator(".video-modal-unsupported [data-copy-stream-url]").Click(); err != nil {
		t.Fatalf("点击复制直链: %v", err)
	}
	copied, err := page.Evaluate("() => (window.__copied || []).join('\\n')", nil)
	if err != nil {
		t.Fatalf("读取剪贴板桩: %v", err)
	}
	s, _ := copied.(string)
	if !strings.Contains(s, "/download?filename=demo.avi") {
		t.Fatalf("复制的直链应指向 /download?filename=demo.avi，got %q", s)
	}

	// 关闭弹层。
	if err := page.Locator(".video-modal-unsupported [data-close]").Click(); err != nil {
		t.Fatalf("关闭兜底弹层: %v", err)
	}

	// ---- MP4：正常 <video>，无兜底弹层 ----
	if err := page.Locator(`tr:has-text("demo.mp4") .file-video-play-btn`).First().Click(); err != nil {
		t.Fatalf("点击 mp4 播放: %v", err)
	}
	if werr := waitLoc(page, ".video-modal video", playwright.WaitForSelectorStateVisible, 8000); werr != nil {
		t.Fatalf("可播格式应渲染 <video>: %v", werr)
	}
	if n, _ := page.Locator(".video-modal-unsupported").Count(); n != 0 {
		t.Fatalf("可播格式不应出现兜底弹层，got %d", n)
	}
	src, attrErr := page.Locator(".video-modal video").GetAttribute("src")
	if attrErr != nil {
		t.Fatalf("读取 <video> src: %v", attrErr)
	}
	if !strings.Contains(src, "/download?filename=demo.mp4") {
		t.Fatalf("<video> src = %q，want 含 /download?filename=demo.mp4", src)
	}
}
