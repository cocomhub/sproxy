// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_share_test.go 覆盖 sclient share 命令族（真二进制 + 子进程）：
// create / list / revoke + 公开 /s/{token} 无签名下载逐字节断言 + 一次性语义。
package sproxy_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// shareCreated 是 `share create --json` 的输出契约（cmd/sclient/output.go JSONFormatter）。
type shareCreated struct {
	Token        string `json:"token"`
	URL          string `json:"url"`
	Filename     string `json:"filename"`
	ExpiresAt    string `json:"expires_at"`
	MaxDownloads int    `json:"max_downloads"`
	OneTime      bool   `json:"one_time"`
}

// shareListResp 是 `share list --json` / GET /api/shares 的响应容器。
type shareListResp struct {
	Shares []client.ShareLink `json:"shares"`
}

// hasShare 报告 shares 中是否存在指定 token。
func hasShare(shares []client.ShareLink, token string) bool {
	for _, s := range shares {
		if s.Token == token {
			return true
		}
	}
	return false
}

// TestE2E_CLI_ShareLifecycle 覆盖分享完整生命周期：
// create → API/公开下载 → list → revoke → 公开路由 404 + 列表移除；并含文件不存在负例。
func TestE2E_CLI_ShareLifecycle(t *testing.T) {
	env := startCLIEnv(t, "")

	content := []byte("shared cli content")
	checksum := sha256hex(content)
	if err := os.WriteFile(filepath.Join(env.TmpDir, "shared.txt"), content, 0644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "upload", "shared.txt")

	// 1) share create --json
	var created shareCreated
	env.sclientJSON(t, env.TmpDir, &created, "share", "create", "shared.txt", "--ttl", "1h")
	if created.Token == "" {
		t.Fatal("share create 应返回非空 token")
	}
	if created.Filename != "shared.txt" {
		t.Fatalf("share create filename 应为 shared.txt, got %q", created.Filename)
	}
	if !strings.Contains(created.URL, "/s/") {
		t.Fatalf("share create URL 应含 /s/, got %q", created.URL)
	}

	// 2) 签名 API：GET /api/shares 含该 token（未过期）
	var apiShares shareListResp
	getJSON(t, env.BaseURL+"/api/shares", &apiShares)
	if !hasShare(apiShares.Shares, created.Token) {
		t.Fatalf("GET /api/shares 应含 token %s, got %+v", created.Token, apiShares.Shares)
	}
	for _, s := range apiShares.Shares {
		if s.Token == created.Token {
			if s.Filename != "shared.txt" {
				t.Fatalf("分享 filename 应为 shared.txt, got %q", s.Filename)
			}
			if s.Expired {
				t.Fatal("刚创建的分享不应已过期")
			}
		}
	}

	// 3) 公开路由无签名下载：字节全等 + checksum 一致 + Content-Disposition 指出文件名
	shareURL := env.BaseURL + "/s/" + created.Token
	status, headers, body := rawGET(t, shareURL)
	if status != http.StatusOK {
		t.Fatalf("公开分享下载应 200, got %d (body: %s)", status, body)
	}
	if string(body) != string(content) {
		t.Fatalf("公开分享下载内容不一致: got %q, want %q", body, content)
	}
	if got := sha256hex(body); got != checksum {
		t.Fatalf("公开分享下载 checksum 不一致: got %s, want %s", got, checksum)
	}
	if cd := headers.Get("Content-Disposition"); !strings.Contains(cd, "shared.txt") {
		t.Fatalf("Content-Disposition 应含 shared.txt, got %q", cd)
	}

	// 4) share list --json 含该 token
	var listed shareListResp
	env.sclientJSON(t, env.TmpDir, &listed, "share", "list")
	if !hasShare(listed.Shares, created.Token) {
		t.Fatalf("share list 应含 token %s, got %+v", created.Token, listed.Shares)
	}

	// 5) share revoke → 公开路由 404 + API 列表不再含该 token
	env.sclient(t, env.TmpDir, "share", "revoke", created.Token)
	if status, _, _ := rawGET(t, shareURL); status != http.StatusNotFound {
		t.Fatalf("revoke 后公开下载应 404, got %d", status)
	}
	var afterRevoke shareListResp
	getJSON(t, env.BaseURL+"/api/shares", &afterRevoke)
	if hasShare(afterRevoke.Shares, created.Token) {
		t.Fatalf("revoke 后 GET /api/shares 不应再含 token %s", created.Token)
	}

	// 6) 负例：分享不存在的文件 → 非零退出，且**未新增任何分享**（副作用断言）
	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "share", "create", "no_such.txt")
	if err == nil {
		t.Fatalf("share create 不存在的文件应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	var afterFailedCreate shareListResp
	getJSON(t, env.BaseURL+"/api/shares", &afterFailedCreate)
	if len(afterFailedCreate.Shares) != 0 {
		t.Fatalf("失败的 share create 不应新增分享, got %+v", afterFailedCreate.Shares)
	}

	// 7) 负例：撤销不存在的 token → 非零退出，且分享列表不变（副作用断言）
	stdout, stderr, err = env.sclientRun(t, env.TmpDir, "share", "revoke", "no-such-token-000000")
	if err == nil {
		t.Fatalf("撤销不存在的 token 应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	var afterFailedRevoke shareListResp
	getJSON(t, env.BaseURL+"/api/shares", &afterFailedRevoke)
	if len(afterFailedRevoke.Shares) != len(afterFailedCreate.Shares) {
		t.Fatalf("失败的 share revoke 不应改变分享数: %d -> %d",
			len(afterFailedCreate.Shares), len(afterFailedRevoke.Shares))
	}
}

// TestE2E_CLI_ShareOneTime 覆盖一次性分享：首次公开下载成功，Consume 后二次访问 404。
func TestE2E_CLI_ShareOneTime(t *testing.T) {
	env := startCLIEnv(t, "")

	content := []byte("one time cli content")
	if err := os.WriteFile(filepath.Join(env.TmpDir, "once.txt"), content, 0644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}
	env.sclient(t, env.TmpDir, "upload", "once.txt")

	var created shareCreated
	env.sclientJSON(t, env.TmpDir, &created, "share", "create", "once.txt", "--one-time")
	if created.Token == "" {
		t.Fatal("一次性分享应返回非空 token")
	}
	if !created.OneTime {
		t.Fatalf("--one-time 应使 one_time 为 true, got %+v", created)
	}

	shareURL := env.BaseURL + "/s/" + created.Token

	// 首次：200 且内容全等
	status, _, body := rawGET(t, shareURL)
	if status != http.StatusOK {
		t.Fatalf("一次性分享首次访问应 200, got %d (body: %s)", status, body)
	}
	if string(body) != string(content) {
		t.Fatalf("一次性分享首次下载内容不一致: got %q, want %q", body, content)
	}

	// 二次：Consume 已删除链接 → 404
	if status, _, _ := rawGET(t, shareURL); status != http.StatusNotFound {
		t.Fatalf("一次性分享二次访问应 404, got %d", status)
	}
}
