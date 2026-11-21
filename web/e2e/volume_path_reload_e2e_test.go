// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// volume_path_reload_e2e_test.go — 回归：在**卷路径**下刷新页面，列表请求必须仍带 `volume`。
//
// 缺陷（2026-11-22 实测）：`refreshList` 早于 `initVolumeSelect` 执行，恢复未完成就把 volume
// 丢弃 ⇒ 刷新后列表为空；点目录（恢复已完成）才正常。修复后列表**等待**恢复再请求。

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// TestVolumePathReload_ListKeepsVolume 锁定「刷新后首个列表请求带 volume」。
func TestVolumePathReload_ListKeepsVolume(t *testing.T) {
	// sproxy:serial: Playwright 单浏览器上下文 + 真实服务端
	baseURL, _, cleanup := testServerCfg(t, func(c *server.Config) {})
	defer cleanup()

	page, stop := pageFixture(t)
	defer stop()

	// 先进入页面，写入「上次在某卷某目录」的持久化状态，再**刷新**（真实复现路径）。
	page.Goto(baseURL + "/ui/")
	if _, err := page.Evaluate(`() => {
		localStorage.setItem('sproxy_volume', 'default');
		localStorage.setItem('sproxy_subdir_vol_default', 'docs');
		localStorage.setItem('sproxy_subdir', 'docs');
	}`, nil); err != nil {
		t.Fatalf("写 localStorage: %v", err)
	}

	listURLs := make(chan string, 16)
	page.On("request", func(r playwright.Request) {
		u := r.URL()
		if strings.Contains(u, "/api/files?") {
			select {
			case listURLs <- u:
			default:
			}
		}
	})

	if _, err := page.Reload(); err != nil {
		t.Fatalf("刷新页面: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case u := <-listURLs:
			if strings.Contains(u, "volume=default") {
				return // 缺陷已修复：刷新后首个列表请求带卷
			}
			t.Fatalf("刷新后列表请求丢失卷：%s", u)
		case <-deadline:
			t.Fatal("刷新后未捕获到列表请求")
		}
	}
}
