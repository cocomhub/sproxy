// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// helpers_e2e_test.go 提供 PR-F2 交互式 Web UI E2E 的共用基建。随各任务按需增补
// （避免引入尚未被引用的 helper 触发 lint `unused`）：
//
//   - testServerCfg：可配置的测试实例启动器（ForceTOTP / CloudDownloadAllowPrivate /
//     自定义 Volumes 等开关），testServer 为它的薄委托，保证既有用例零回归。
//   - waitLoc：以 Locator.WaitFor 替代已废弃的 Page.WaitForSelector（lint SA1019）。
//
// 无凭据前提与既有 testServer 完全一致（CredentialTTL=-1 → ring 空 +
// AllowInsecureLoopback=true → loopback 兜底放行），保证既有用例语义不变。

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/mxschmitt/playwright-go"
)

// testServerCfg 启动 sproxy 测试实例，允许调用方在启动前修改 cfg——用于
// ForceTOTP=true（TOTP 注册）、CloudDownloadAllowPrivate=true（回环云下载源）、
// 自定义 Volumes 等。无凭据前提与 testServer 完全一致（CredentialTTL=-1 +
// AllowInsecureLoopback=true），保证既有用例语义零回归。
func testServerCfg(t *testing.T, mutate func(cfg *server.Config)) (string, *server.Config, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	cfg := server.Default()
	cfg.StorageRoot = tmpDir
	cfg.LogLevel = "error"
	// UI E2E 为无凭据场景（页面不带 SproxySig 凭据）：禁首启 anonymous 生成
	// （CredentialTTL=-1 → ring 空），并允许 loopback 无认证兜底（httptest 天然
	// 127.0.0.1）——v2 skey-id 必传下 Web 请求不带凭据即 401，UI E2E 需此兜底。
	cfg.CredentialTTL = -1
	cfg.AllowInsecureLoopback = true
	if mutate != nil {
		mutate(cfg)
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
	return ts.URL, cfg, func() {
		ts.Close()
		h.Close()
		// 清理云端下载目录，防止 TempDir RemoveAll 失败
		os.RemoveAll(filepath.Join(tmpDir, ".__cloud__"))
		os.RemoveAll(filepath.Join(tmpDir, ".__downloads__"))
	}
}

// waitLoc 用 locator-based 等待替代已废弃的 Page.WaitForSelector（strict=false：
// First() 取首个，等价旧 Page.WaitForSelector 的多匹配语义）。state 传 nil 表示
// Playwright 默认（visible）。
func waitLoc(page playwright.Page, selector string, state *playwright.WaitForSelectorState, timeoutMs float64) error {
	return page.Locator(selector).First().WaitFor(playwright.LocatorWaitForOptions{
		State:   state,
		Timeout: playwright.Float(timeoutMs),
	})
}
