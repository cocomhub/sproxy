// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// meta_extra_e2e_test.go Playwright e2e：文件元数据 extra 展示与编辑（2026-10-26 读闭环片 4）。
//
// 场景：上传带 meta_extra 的文件 → 列表行显示 meta 徽标 → 点「元数据」→ 弹窗显示 KV →
// 编辑 JSON → 保存（合并）→ 断言读区更新；再删键 → 断言键消失。

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// TestMetaExtra_Panel 元数据弹窗：显示 + 合并保存 + 删键。
func TestMetaExtra_Panel(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}

	// 上传带 meta_extra 的文件（X-File-Meta-Extra 头：raw JSON）。
	content := []byte("meta-extra-e2e")
	if st, _ := seedUploadWithMeta(t, baseURL, "meta-extra.txt", content, `{"code":"E2E-001","maker":"STUDIO E2E"}`); st != http.StatusOK {
		t.Fatalf("seed upload = %d", st)
	}

	// 点「元数据」按钮 → 弹窗显示 KV（code / E2E-001 / maker / STUDIO E2E）。
	// （列表行 meta 徽标依赖服务端 list 返回 extra——当前 DTO 不含，徽标逻辑保留零回归。）
	if err := page.Locator(".file-meta-btn").First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click meta btn: %v", err)
	}
	if err := waitLoc(page, "#meta-modal", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("meta-modal 未显示: %v", err)
	}
	readTxt, _ := page.Locator("#meta-read-area").InnerText()
	if !strings.Contains(readTxt, "code") || !strings.Contains(readTxt, "E2E-001") {
		t.Fatalf("读区应显示 code=E2E-001: %q", readTxt)
	}
	if !strings.Contains(readTxt, "maker") || !strings.Contains(readTxt, "STUDIO E2E") {
		t.Fatalf("读区应显示 maker: %q", readTxt)
	}

	// 编辑 JSON：把 code 改为 E2E-002（合并保存应保留 maker）。
	edit := page.Locator("#meta-edit-json")
	if err := edit.Fill(`{"code":"E2E-002"}`, playwright.LocatorFillOptions{}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := page.Locator("#meta-save-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click save: %v", err)
	}
	// 保存后弹窗刷新 → 读区应显示新 code + 保留 maker（合并语义）。
	// 注意：读区在弹窗打开期间一直可见 ⇒ 不能用 WaitForSelectorStateVisible（会立即
	// 返回刷新前旧 DOM）⇒ 必须轮询**文本内容**变化。
	readTxt2 := waitReadAreaContains(t, page, "E2E-002", 8000)
	if !strings.Contains(readTxt2, "STUDIO E2E") {
		t.Fatalf("合并后应保留旧 maker（默认新增非覆盖）: %q", readTxt2)
	}

	// 删键：把 edit 填为待删键 code → 点「删除键」→ 读区 code 消失、maker 保留。
	if err := edit.Fill(`{"code":null}`, playwright.LocatorFillOptions{}); err != nil {
		t.Fatalf("fill delete: %v", err)
	}
	if err := page.Locator("#meta-delete-key-btn").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click delete: %v", err)
	}
	// 轮询文本：**先等 code 消失**（删键后的确定性信号），再断言 maker 保留。
	// 不能等 maker——maker 在删键前的旧 DOM 里就存在，contains 会立即返回旧值。
	readTxt3 := waitReadAreaAbsent(t, page, "E2E-002", 8000)
	if !strings.Contains(readTxt3, "STUDIO E2E") {
		t.Fatalf("删键后 maker 应保留: %q", readTxt3)
	}

	// 关闭弹窗（×）→ 隐藏。
	if err := page.Locator("#meta-modal-close").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click close: %v", err)
	}
	if err := waitLoc(page, "#meta-modal", playwright.WaitForSelectorStateHidden, 8000); err != nil {
		t.Fatalf("关闭后弹窗应隐藏: %v", err)
	}
}

// TestMetaExtra_Empty 无 meta 的文件：点元数据 → 弹窗显示「暂无元数据」占位。
func TestMetaExtra_Empty(t *testing.T) {
	baseURL, _, cleanup := testServer(t)
	defer cleanup()
	page, stop := pageFixture(t)
	defer stop()

	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	if st, _ := seedUploadMultipart(t, baseURL, "", "plain.txt", []byte("plain")); st != http.StatusOK {
		t.Fatalf("seed = %d", st)
	}
	if err := page.Locator(".file-meta-btn").First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(8000)}); err != nil {
		t.Fatalf("click meta btn: %v", err)
	}
	if err := waitLoc(page, "#meta-read-area", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("读区: %v", err)
	}
	readTxt, _ := page.Locator("#meta-read-area").InnerText()
	if !strings.Contains(readTxt, "暂无元数据") {
		t.Fatalf("空 meta 应显示占位: %q", readTxt)
	}
}

// waitReadAreaContains 轮询 #meta-read-area 文本直到包含 want（或超时失败）。
// 为什么不用 waitLoc(...Visible)：读区在弹窗打开期间**一直可见**，visible 判定会立即
// 返回刷新前的旧 DOM（本用例曾因此读到旧值而假红）。
func waitReadAreaContains(t *testing.T, page playwright.Page, want string, timeoutMs float64) string {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	var last string
	for time.Now().Before(deadline) {
		txt, err := page.Locator("#meta-read-area").InnerText()
		if err == nil {
			last = txt
			if strings.Contains(txt, want) {
				return txt
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("读区在 %v ms 内未出现 %q，最后内容: %q", timeoutMs, want, last)
	return last
}

// waitReadAreaAbsent 轮询 #meta-read-area 文本直到**不再包含** banned（或超时失败）。
// 与 waitReadAreaContains 相对：用于「删键后旧值消失」的确定性信号。
func waitReadAreaAbsent(t *testing.T, page playwright.Page, banned string, timeoutMs float64) string {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	var last string
	for time.Now().Before(deadline) {
		txt, err := page.Locator("#meta-read-area").InnerText()
		if err == nil {
			last = txt
			if !strings.Contains(txt, banned) {
				return txt
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("读区在 %v ms 内仍含 %q，最后内容: %q", timeoutMs, banned, last)
	return last
}
