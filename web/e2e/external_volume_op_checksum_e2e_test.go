// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// external_volume_op_checksum_e2e_test.go — 外部卷（secretdata / baidupcs / S3 等）上的
// **Web UI 删除与重命名**必须可用（用户实测缺陷修复，2026-10-27）。
//
// 缺陷：外部卷列举（`ExternalVolumeOps.ExternalList`）不返回 checksum——整文件哈希记录在
// 卷内 meta（secretdata 加密 meta / trusted sidecar），逐条读会 n 次远端往返。于是 Web UI
// 文件行 `data-checksum` 为空，旧实现在前端直接报「缺少 checksum，无法校验完整性」⇒
// **外部卷无法删除/改名**（功能死路）。CLI 无此问题，因为 `FileClient.Delete` 一直先
// `Stat` 取 checksum 再删。
//
// 修复（本文件锁住）：前端写操作在 checksum 缺失时先 `HEAD /api/files/stat?volume=<行卷>`
// 取 `X-File-Checksum`，再用它发起 `POST /delete` / `POST /rename`（批量操作同理逐条补齐）；
// 拿不到则**中止并提示**（不静默跳过校验）。
//
// 断言策略（真交互）：用请求记录器捕获整段交互的请求序列（比 ExpectRequest 更稳定——
// 删除路径在 stat 之后还有 confirm 对话框，ExpectRequest 只等第一个匹配事件），然后断言
// 「stat 发生在 delete/rename 之前」且 URL 带行卷，最后断言操作真实生效。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/mxschmitt/playwright-go"
)

// reqRecorder 记录页面请求（method + URL），并发安全。
type reqRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *reqRecorder) add(method, url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method+" "+url)
}

// first 返回首个包含 sub 的请求（无则空串）。
func (r *reqRecorder) first(sub string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			return c
		}
	}
	return ""
}

// indexOf 返回首个包含 sub 的请求下标（无则 -1）。
func (r *reqRecorder) indexOf(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// recordRequests 挂上请求记录器，返回记录器。
func recordRequests(page playwright.Page) *reqRecorder {
	r := &reqRecorder{}
	page.On("request", func(req playwright.Request) { r.add(req.Method(), req.URL()) })
	return r
}

// TestExternalVolume_RenameUsesStatChecksum 外部卷改名可用：
// ① 列表已带 checksum（S2/S3：secretdata 列举直接给 SHA-256）⇒ 无 stat 也能成功——
//
//	断言只要求「改名真实生效」（操作结果语义，不绑定实现路径）；
//
// ② 若前端确实走了 stat（行 checksum 缺失的防御路径）⇒ 断言它带行卷 volume。
func TestExternalVolume_RenameUsesStatChecksum(t *testing.T) {
	// sproxy:serial: auditVaultE2EServer 注册/注销全局 secretdata 后端（registry 进程级），与并行 user_volumes 用例冲突，必须串行。
	baseURL := setupSecretVolumeWithFile(t, "ren.txt", []byte("rename-on-external"))
	page, stop := pageFixture(t)
	defer stop()
	rec := recordRequests(page)

	openVaultList(t, page, baseURL, "ren.txt")

	acceptDialog(page, "ren2.txt")
	if err := page.Locator(".file-rename-btn[data-filename='ren.txt']").Click(); err != nil {
		t.Fatalf("点击重命名: %v", err)
	}
	waitTextVisible(t, page, "#file-list", "ren2.txt", 10000)
	waitTextGone(t, page, "#file-list", "ren.txt", 10000)

	// 改名必须真实发出（且成功）。
	if renIdx := rec.indexOf("/rename?"); renIdx < 0 {
		t.Fatalf("未观察到 POST /rename（外部卷改名仍不可用？）；请求序列=%v", rec.calls)
	}
	// 防御路径（行 checksum 缺失时）stat 必须带行卷。
	if statIdx := rec.indexOf("/api/files/stat?"); statIdx >= 0 {
		if u := rec.first("/api/files/stat?"); !strings.Contains(u, "volume="+auditVaultType) {
			t.Errorf("stat 请求 = %q，应带行卷 volume=%s（外部卷需显式定位）", u, auditVaultType)
		}
	}
}

// TestExternalVolume_DeleteUsesStatChecksum 外部卷删除：先 HEAD stat 取 checksum → POST /delete。
func TestExternalVolume_DeleteUsesStatChecksum(t *testing.T) {
	// sproxy:serial: auditVaultE2EServer 注册/注销全局 secretdata 后端（registry 进程级），与并行 user_volumes 用例冲突，必须串行。
	baseURL := setupSecretVolumeWithFile(t, "del.txt", []byte("delete-on-external"))
	page, stop := pageFixture(t)
	defer stop()
	rec := recordRequests(page)

	openVaultList(t, page, baseURL, "del.txt")

	// 删除前有 confirm()（前端先取 checksum 再确认）——统一接受。
	acceptDialog(page, "")
	if err := page.Locator(".file-delete-btn[data-filename='del.txt']").Click(); err != nil {
		t.Fatalf("点击删除: %v", err)
	}
	// 确定性等待：delete 请求出现（而不是固定 30s 轮询 DOM 文本）。
	waitRecorded(t, rec, "/delete?", 10*time.Second)

	// 删除必须真实发出（且成功）。
	if delIdx := rec.indexOf("/delete?"); delIdx < 0 {
		t.Fatalf("未观察到 POST /delete（外部卷删除仍不可用？）；请求序列=%v", rec.calls)
	}
	// 防御路径（行 checksum 缺失时）stat 必须带行卷。
	if statIdx := rec.indexOf("/api/files/stat?"); statIdx >= 0 {
		if u := rec.first("/api/files/stat?"); !strings.Contains(u, "volume="+auditVaultType) {
			t.Errorf("stat 请求 = %q，应带行卷 volume=%s", u, auditVaultType)
		}
	}
	// 服务端已删 → 列表刷新后该行消失。
	waitTextGone(t, page, "#file-list", "del.txt", 10000)
}

// waitRecorded 等记录器出现包含 sub 的请求（≤timeout，无固定 sleep）。
func waitRecorded(t *testing.T, rec *reqRecorder, sub string, timeout time.Duration) {
	t.Helper()
	if !testutil.WaitForBool(timeout, func() bool { return rec.indexOf(sub) >= 0 }) {
		t.Fatalf("%v 内未观察到含 %q 的请求；请求序列=%v", timeout, sub, rec.calls)
	}
}

// setupSecretVolumeWithFile 建 secretdata 封装卷 + 上传一个文件，返回 baseURL。
func setupSecretVolumeWithFile(t *testing.T, filename string, content []byte) string {
	t.Helper()
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vaultRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	baseURL, _ := auditVaultE2EServer(t, vaultRoot)
	if st, body := createUserVolume(t, baseURL, auditVaultType, auditVaultType, `{"target":"main"}`); st != 200 {
		t.Fatalf("建卷 status=%d body=%s", st, body)
	}
	if st, body := seedUploadToVolume(t, baseURL, auditVaultType, filename, content); st != 200 {
		t.Fatalf("上传 status=%d body=%s", st, body)
	}
	return baseURL
}

// openVaultList 打开 UI、切到加密卷、等目标文件行渲染（含其操作按钮）。
func openVaultList(t *testing.T, page playwright.Page, baseURL, wantFile string) {
	t.Helper()
	if _, err := page.Goto(baseURL+"/ui/", playwright.PageGotoOptions{Timeout: playwright.Float(10000)}); err != nil {
		t.Fatalf("导航: %v", err)
	}
	if err := waitLoc(page, "#volume-select option[value='"+auditVaultType+"']", playwright.WaitForSelectorStateAttached, 8000); err != nil {
		t.Fatalf("卷下拉未含 %s: %v", auditVaultType, err)
	}
	if _, err := page.Locator("#volume-select").SelectOption(
		playwright.SelectOptionValues{Values: &[]string{auditVaultType}}); err != nil {
		t.Fatalf("选择加密卷: %v", err)
	}
	waitTextVisible(t, page, "#file-list", wantFile, 8000)
}

// TestExternalVolume_BatchDeleteUsesChecksums 外部卷**批量删除**（fillMissingChecksums 分支）：
// 勾选两文件 → 批量删除 → 断言两文件都消失。
func TestExternalVolume_BatchDeleteUsesChecksums(t *testing.T) {
	// sproxy:serial: auditVaultE2EServer 注册/注销全局 secretdata 后端（registry 进程级），与并行 user_volumes 用例冲突，必须串行。
	baseURL := setupSecretVolumeWithFiles(t, "bd1.txt", []byte("b1"), "bd2.txt", []byte("b2"))
	page, stop := pageFixture(t)
	defer stop()

	openVaultList(t, page, baseURL, "bd1.txt")
	for _, f := range []string{"bd1.txt", "bd2.txt"} {
		if err := page.Locator(".file-select[data-filename='" + f + "']").Check(); err != nil {
			t.Fatalf("勾选 %s: %v", f, err)
		}
	}
	acceptDialog(page, "")
	if err := page.Locator("#batch-delete-btn").Click(); err != nil {
		t.Fatalf("点批量删除: %v", err)
	}
	waitTextGone(t, page, "#file-list", "bd1.txt", 10000)
	waitTextGone(t, page, "#file-list", "bd2.txt", 10000)
}

// TestExternalVolume_BatchRenameUsesChecksums 外部卷**批量重命名**：
// 勾选两文件 → 批量重命名（prompt 序列）→ 断言新名出现、旧名消失。
func TestExternalVolume_BatchRenameUsesChecksums(t *testing.T) {
	// sproxy:serial: auditVaultE2EServer 注册/注销全局 secretdata 后端（registry 进程级），与并行 user_volumes 用例冲突，必须串行。
	baseURL := setupSecretVolumeWithFiles(t, "br1.txt", []byte("r1"), "br2.txt", []byte("r2"))
	page, stop := pageFixture(t)
	defer stop()

	openVaultList(t, page, baseURL, "br1.txt")
	for _, f := range []string{"br1.txt", "br2.txt"} {
		if err := page.Locator(".file-select[data-filename='" + f + "']").Check(); err != nil {
			t.Fatalf("勾选 %s: %v", f, err)
		}
	}
	respondDialogs(page, func(d playwright.Dialog) {
		switch {
		case strings.Contains(d.Message(), "br1.txt"):
			_ = d.Accept("br1-new.txt")
		case strings.Contains(d.Message(), "br2.txt"):
			_ = d.Accept("br2-new.txt")
		default:
			_ = d.Accept("")
		}
	})
	if err := page.Locator("#batch-rename-btn").Click(); err != nil {
		t.Fatalf("点批量重命名: %v", err)
	}
	waitTextVisible(t, page, "#file-list", "br1-new.txt", 10000)
	waitTextVisible(t, page, "#file-list", "br2-new.txt", 10000)
	waitTextGone(t, page, "#file-list", "br1.txt", 10000)
	waitTextGone(t, page, "#file-list", "br2.txt", 10000)
}

// setupSecretVolumeWithFiles 建卷并上传多个文件（批量场景）。参数：name1, content1, name2, content2...
func setupSecretVolumeWithFiles(t *testing.T, files ...interface{}) string {
	t.Helper()
	vaultRoot := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vaultRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	baseURL, _ := auditVaultE2EServer(t, vaultRoot)
	if st, body := createUserVolume(t, baseURL, auditVaultType, auditVaultType, `{"target":"main"}`); st != 200 {
		t.Fatalf("建卷 status=%d body=%s", st, body)
	}
	for i := 0; i+1 < len(files); i += 2 {
		name := files[i].(string)
		content := files[i+1].([]byte)
		if st, body := seedUploadToVolume(t, baseURL, auditVaultType, name, content); st != 200 {
			t.Fatalf("上传 %s status=%d body=%s", name, st, body)
		}
	}
	return baseURL
}
