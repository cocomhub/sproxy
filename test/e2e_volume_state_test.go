// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package sproxy_test

// e2e_volume_state_test.go 是**卷状态缓存**（设计 2026-10-28-volume-state-maintenance）
// 的端到端验证：真实 sproxy 二进制 + 真实 sclient CLI + 真实 WebDAV 外部卷后端。
//
// 锁定的契约（缺一即功能不可用）：
//  1. 启用后外部卷目录列举走缓存（外部直改在 TTL 内**不可见**）；
//  2. 经 API/CLI 的写入**自动失效**（上传/删除后 list 立即可见变化 —— 无需调用方登记）；
//  3. 显式 `POST /api/volumes/refresh` 立即收敛（外部改动可见）；
//  4. 未启用（默认）时无缓存（外部直改立即可见）—— 证明开关语义与「缓存确实生效」。
//
// 对照关系：TestVolumeStateCache_DisabledSeesExternalChangeImmediately 是**对照组**；
// 若两个测试行为相同，说明缓存没生效（假绿）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/webdav"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// e2eWebDAVUser / e2eWebDAVPass 是测试 WebDAV 服务的 BasicAuth 凭据
// （webdav 后端 fail-closed：无匿名目标，必须配置 username+password 或 token）。
const (
	e2eWebDAVUser = "e2e-dav"
	e2eWebDAVPass = "e2e-dav-pass"
)

// startWebDAVBackend 起一个本地 WebDAV 服务（本地目录支撑 + BasicAuth），返回 (URL, 根目录)。
func startWebDAVBackend(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dav := &webdav.Handler{
		Prefix:     "/dav",
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != e2eWebDAVUser || p != e2eWebDAVPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="e2e"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/dav", dir
}

// signedJSON 用 SproxySig 签名发送 JSON 请求（e2e 测试凭据）。
func signedJSON(t *testing.T, baseURL, method, path string, payload any) (int, []byte) {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	sproxysig.SignRequestWithSkeyID(req, e2eTestAK, e2eTestID, e2eTestSK)
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// createWebDAVVolume 运行期创建 webdav 外部卷（真实用户路径：POST /api/volumes/user）。
func createWebDAVVolume(t *testing.T, baseURL, name, davURL string) {
	t.Helper()
	st, body := signedJSON(t, baseURL, http.MethodPost, "/api/volumes/user", map[string]any{
		"name": name, "type": "webdav", "unlimited": true,
		"extra": map[string]any{
			"url": davURL, "username": e2eWebDAVUser, "password": e2eWebDAVPass,
		},
	})
	if st != http.StatusOK {
		t.Fatalf("创建 webdav 卷 %q status=%d body=%s", name, st, body)
	}
}

// prepareWebDAVRoots 预建 sproxy 外部卷键空间父目录（`<owner>/user`）：
// x/net/webdav 的 MKCOL 不递归创建父目录（真实 WebDAV 服务端多会自动建，本测试服务端不建）。
func prepareWebDAVRoots(t *testing.T, davRoot string) {
	t.Helper()
	// 独享网络卷（owner-only ACL）键空间**无 owner 前缀**：卷根即 `/dav`，用户文件在
	// `user/` 桶下（见 volume.Shared / externalVolumeKey）。
	for _, p := range []string{
		filepath.Join(davRoot, "user"),
		filepath.Join(davRoot, e2eTestAK, "user"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("预建 WebDAV 目录: %v", err)
		}
	}
}

// listVolumeNames 列出指定卷的条目名集合（GET /api/files?volume=<卷>）。
func listVolumeNames(t *testing.T, baseURL, vol string) map[string]bool {
	t.Helper()
	var out struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	getJSON(t, baseURL+"/api/files?volume="+vol, &out)
	names := make(map[string]bool, len(out.Files))
	for _, f := range out.Files {
		names[f.Name] = true
	}
	return names
}

// findFileUnder 在 root 下递归查找指定 basename，返回其父目录（找不到返回 ""）。
func findFileUnder(t *testing.T, root, base string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil //nolint:nilerr // 遍历容错
		}
		if !d.IsDir() && d.Name() == base {
			found = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// TestVolumeStateCache_ExternalVolumeE2E 端到端：外部卷 list 缓存 + 写穿失效 + 显式 refresh。
func TestVolumeStateCache_ExternalVolumeE2E(t *testing.T) {
	t.Parallel()
	davURL, davRoot := startWebDAVBackend(t)
	// TTL 设长：确保断言窗口内不因时间过期而回源（缓存命中/失效必须由机制保证）。
	env := startCLIEnv(t, "volume_state:\n  enabled: true\n  ttl: 10m\n")
	createWebDAVVolume(t, env.BaseURL, "extdav", davURL)
	prepareWebDAVRoots(t, davRoot)

	// ---- 1) 首次 list（无 a.bin）——填充缓存 ----
	if got := listVolumeNames(t, env.BaseURL, "extdav"); got["a.bin"] {
		t.Fatalf("新建 webdav 卷不应含 a.bin，got %v", got)
	}

	// ---- 2) 经真实 CLI 上传 ⇒ 写后自动失效 ⇒ list 立即可见 ----
	if werr := os.WriteFile(filepath.Join(env.TmpDir, "a.bin"), []byte("alpha-content"), 0o600); werr != nil {
		t.Fatalf("写本地文件: %v", werr)
	}
	env.sclient(t, env.TmpDir, "upload", "a.bin", "extdav:/")
	if got := listVolumeNames(t, env.BaseURL, "extdav"); !got["a.bin"] {
		t.Fatalf("上传后 list 应含 a.bin（写穿失效未生效），got %v", got)
	}

	// ---- 3) 外部直改（绕过 sproxy）⇒ 缓存命中 ⇒ list 仍只见 a.bin ----
	davDir := findFileUnder(t, davRoot, "a.bin")
	if davDir == "" {
		t.Fatalf("未在 WebDAV 根 %s 下找到 a.bin（上传未落到后端）", davRoot)
	}
	if werr := os.WriteFile(filepath.Join(davDir, "b.bin"), []byte("bravo-direct"), 0o600); werr != nil {
		t.Fatalf("直写 WebDAV 后端: %v", werr)
	}
	if got := listVolumeNames(t, env.BaseURL, "extdav"); got["b.bin"] {
		t.Fatalf("TTL 内外部直改应**不可见**（缓存未生效，list 已回源），got %v", got)
	}

	// ---- 4) 显式 refresh ⇒ 立即收敛 ----
	if st, body := signedJSON(t, env.BaseURL, http.MethodPost, "/api/volumes/refresh?volume=extdav", nil); st != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", st, body)
	}
	if got := listVolumeNames(t, env.BaseURL, "extdav"); !got["b.bin"] || !got["a.bin"] {
		t.Fatalf("refresh 后 list 应见 a.bin+b.bin，got %v", got)
	}

	// ---- 5) CLI 删除 ⇒ 写后自动失效 ⇒ list 立即不含 ----
	env.sclient(t, env.TmpDir, "delete", "extdav:/a.bin")
	if got := listVolumeNames(t, env.BaseURL, "extdav"); got["a.bin"] {
		t.Fatalf("删除后 list 不应含 a.bin（失效未生效），got %v", got)
	}
}

// TestVolumeStateCache_DisabledSeesExternalChangeImmediately 是**对照组**：未启用缓存
// （默认配置）时外部直改立即可见 —— 与启用组的行为差异证明缓存真的生效（防假绿）。
func TestVolumeStateCache_DisabledSeesExternalChangeImmediately(t *testing.T) {
	t.Parallel()
	davURL, davRoot := startWebDAVBackend(t)
	env := startCLIEnv(t, "") // 不启用 volume_state（默认 false）
	createWebDAVVolume(t, env.BaseURL, "extdav", davURL)
	prepareWebDAVRoots(t, davRoot)

	if werr := os.WriteFile(filepath.Join(env.TmpDir, "a.bin"), []byte("alpha"), 0o600); werr != nil {
		t.Fatalf("写本地文件: %v", werr)
	}
	env.sclient(t, env.TmpDir, "upload", "a.bin", "extdav:/")

	davDir := findFileUnder(t, davRoot, "a.bin")
	if davDir == "" {
		t.Fatalf("未在 WebDAV 根 %s 下找到 a.bin", davRoot)
	}
	if werr := os.WriteFile(filepath.Join(davDir, "b.bin"), []byte("bravo"), 0o600); werr != nil {
		t.Fatalf("直写 WebDAV 后端: %v", werr)
	}
	// 未启用 ⇒ 每次 list 回源 ⇒ 立即可见（这正是启用组必须**看不到**的行为）。
	// 后端是本地文件系统（无传播延迟），无需轮询等待。
	if got := listVolumeNames(t, env.BaseURL, "extdav"); !got["b.bin"] {
		t.Fatalf("未启用缓存时外部直改应立即可见（list 每次回源），got %v", got)
	}
}
