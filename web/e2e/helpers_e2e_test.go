// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// helpers_e2e_test.go 提供 PR-F2 交互式 Web UI E2E 的共用基建。随各任务按需增补
// （避免引入尚未被引用的 helper 触发 lint `unused`）：
//
//   - testServerCfg：可配置的测试实例启动器（ForceTOTP / CloudDownloadAllowPrivate /
//     自定义 Volumes 等开关），testServer 为它的薄委托，保证既有用例零回归。
//   - waitLoc：以 Locator.WaitFor 替代已废弃的 Page.WaitForSelector（lint SA1019）。
//   - waitTextGone / waitTextVisible：轮询容器文本直到目标词消失/出现——断言「变化后的
//     DOM」而非「元素存在」。
//
// 无凭据前提与既有 testServer 完全一致（CredentialTTL=-1 → ring 空 +
// AllowInsecureLoopback=true → loopback 兜底放行），保证既有用例语义不变。

import (
	"encoding/json"
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

// respondDialogs 安装 page 级 dialog 处理器（常驻）。Playwright 默认 auto-dismiss，
// 未装处理器时 confirm() 返回 false → 点击 inert；所有 confirm/prompt 流程必须先装。
// 序列 dialog（批重命名 N 个 prompt）在 respond 内按 d.Message() 分派（Playwright
// 串行派发 dialog 事件，按消息无状态分派即安全）。
func respondDialogs(page playwright.Page, respond func(d playwright.Dialog)) {
	page.OnDialog(func(d playwright.Dialog) { respond(d) })
}

// acceptDialog 对每个 dialog 一律 Accept（prompt 时写入 promptText）。
func acceptDialog(page playwright.Page, promptText string) {
	respondDialogs(page, func(d playwright.Dialog) { _ = d.Accept(promptText) })
}

// requestJSON 解析捕获到的请求体 JSON 到 v。
func requestJSON(t *testing.T, req playwright.Request, v any) {
	t.Helper()
	b, err := req.PostDataBuffer()
	if err != nil {
		t.Fatalf("读取请求体: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("解析请求体 JSON: %v (body=%q)", err, string(b))
	}
}

// waitTextGone 轮询 sel 容器的 InnerText，直到不再包含 want（≤timeout）。
// 用于断言删除/重命名/切目录后的行消失（避免只断元素存在）。
func waitTextGone(t *testing.T, page playwright.Page, sel, want string, timeoutMs float64) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		txt, err := page.Locator(sel).InnerText()
		if err == nil && !strings.Contains(txt, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("文本 %q 未在 %.0fms 内从 %s 消失（疑似未接线）", want, timeoutMs, sel)
}

// waitTextVisible 轮询 sel 容器的 InnerText，直到包含 want（≤timeout）。
// 用于断言轮询类流程（如云下载每 3s 刷新）后出现的状态文案。
func waitTextVisible(t *testing.T, page playwright.Page, sel, want string, timeoutMs float64) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		txt, err := page.Locator(sel).InnerText()
		if err == nil && strings.Contains(txt, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	txt, _ := page.Locator(sel).InnerText()
	t.Fatalf("文本 %q 未在 %.0fms 内出现在 %s 中（疑似未接线）；当前文本: %q", want, timeoutMs, sel, txt)
}
