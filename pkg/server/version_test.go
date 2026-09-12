// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestVersion_ListVersions_Disabled(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("test content")
	uploadFile(t, url, "test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	resp, err := http.Get(url + "/api/versions?filename=test.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Versioning disabled by default
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 for disabled versioning, got %d", resp.StatusCode)
	}
}

func TestVersion_ListVersions_NoVersions(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	resp, err := http.Get(url + "/api/versions?filename=nonexistent.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Versions []any `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Versions) != 0 {
		t.Fatalf("expected empty versions, got %d", len(result.Versions))
	}
}

func TestVersion_CreateAndList(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// Upload first version
	body1 := []byte("version 1")
	uploadFile(t, url, "ver.txt", body1, map[string]string{
		"X-File-Checksum": sha256hex(body1),
	})

	// Upload second version (overwrite)
	body2 := []byte("version 2")
	uploadFile(t, url, "ver.txt", body2, map[string]string{
		"X-File-Checksum": sha256hex(body2),
	})

	// List versions
	resp, err := http.Get(url + "/api/versions?filename=ver.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result struct {
		Versions []VersionInfo `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Versions) == 0 {
		t.Fatal("expected at least 1 version")
	}
}

func TestVersion_Restore(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// Upload first version
	body1 := []byte("version one")
	uploadFile(t, url, "restore.txt", body1, map[string]string{
		"X-File-Checksum": sha256hex(body1),
	})

	// Upload second version
	body2 := []byte("version two")
	uploadFile(t, url, "restore.txt", body2, map[string]string{
		"X-File-Checksum": sha256hex(body2),
	})

	// List versions
	resp, err := http.Get(url + "/api/versions?filename=restore.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&listResult); err != nil {
		t.Fatal(err)
	}
	if len(listResult.Versions) == 0 {
		t.Fatal("expected versions")
	}

	// Restore first version
	versionID := listResult.Versions[0].VersionID
	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=restore.txt&version_id=%d", url, versionID)
	resp2, err := http.Post(restoreURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on restore, got %d", resp2.StatusCode)
	}
}

func TestVersion_MissingFilename(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
	})

	resp, err := http.Get(url + "/api/versions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- deleteVersionHandler tests ----

func TestDeleteVersion_Disabled(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("DELETE", url+"/api/versions?filename=test.txt&version_id=12345", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 for disabled versioning, got %d", resp.StatusCode)
	}
}

func TestDeleteVersion_NoFilename(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
	})

	req, err := http.NewRequest("DELETE", url+"/api/versions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDeleteVersion_HappyPath(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// Upload a file
	body := []byte("delete version test")
	uploadFile(t, url, "delver.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// Overwrite to create a version
	body2 := []byte("delete version test v2")
	uploadFile(t, url, "delver.txt", body2, map[string]string{
		"X-File-Checksum": sha256hex(body2),
	})

	// List versions to get a version_id
	resp, err := http.Get(url + "/api/versions?filename=delver.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&listResult); err != nil {
		t.Fatal(err)
	}
	if len(listResult.Versions) == 0 {
		t.Fatal("expected at least one version")
	}

	versionID := listResult.Versions[0].VersionID

	// Delete the version
	delURL := fmt.Sprintf("%s/api/versions?filename=delver.txt&version_id=%d", url, versionID)
	req, err := http.NewRequest("DELETE", delURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on delete version, got %d", resp.StatusCode)
	}

	var delResult UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&delResult); err != nil {
		t.Fatal(err)
	}
	if !delResult.Success {
		t.Fatalf("delete version failed: %s", delResult.Message)
	}
}

func TestDeleteVersion_NonExistent(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
	})

	req, err := http.NewRequest("DELETE", url+"/api/versions?filename=nonexistent.txt&version_id=99999", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent version, got %d", resp.StatusCode)
	}
}

// ---- restoreVersionHandler tests ----

func TestRestoreVersionHandler_DisabledVersioning(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	cfg := cfgPtr.Load()
	cfg.Versioning.Enabled = false
	cfgPtr.Store(cfg)

	resp, err := http.Post(url+"/api/versions/restore?filename=test.txt&version_id=1", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("expected 501 for disabled versioning, got %d", resp.StatusCode)
	}
}

func TestRestoreVersionHandler_MissingParams(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/versions/restore?version_id=1", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing filename, got %d", resp.StatusCode)
	}

	resp, err = http.Post(url+"/api/versions/restore?filename=test.txt", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing version_id, got %d", resp.StatusCode)
	}
}

// TestVersion_NewLayout 验证 version 迁移到 Tenant API 后的新布局：
// 版本文件落 <root>/alice/version/dir/f.txt/<versionID>，checksum key =
// "version/dir/f.txt/<id>"（per-tenant store，无 owner 前缀），旧 __version__
// 前缀 key 不复存在（R4 碰撞消除）。versioning 缝隙修复：新布局 upload 覆盖时
// saveVersionBeforeOverwrite 必须能保存版本（此前走 safePathFor 旧布局被静默跳过）。
func TestVersion_NewLayout(t *testing.T) {
	env := newOwnerUploadEnv(t)
	cfg := env.h.cfgPtr.Load()
	cfg.Versioning.Enabled = true
	cfg.Versioning.MaxVersions = 10
	env.h.cfgPtr.Store(cfg)

	// 首次上传 user/dir/f.txt
	body1 := []byte("version 1")
	status, respBody := uploadFile(t, env.urls["alice"], "dir/f.txt", body1, map[string]string{
		"X-File-Checksum": sha256hex(body1),
		"X-File-Path":     "dir/f.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("首次上传应成功: %d %s", status, respBody)
	}

	// 覆盖 → 触发 saveVersionBeforeOverwrite 保存旧版本
	body2 := []byte("version 2")
	status, respBody = uploadFile(t, env.urls["alice"], "dir/f.txt", body2, map[string]string{
		"X-File-Checksum": sha256hex(body2),
		"X-File-Path":     "dir/f.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("覆盖上传应成功: %d %s", status, respBody)
	}

	// 版本文件在 <root>/alice/version/dir/f.txt/<versionID>
	verDir := filepath.Join(env.root, "alice", "version", "dir", "f.txt")
	entries, err := os.ReadDir(verDir)
	if err != nil {
		t.Fatalf("读取版本目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应恰好 1 个版本文件, got %d", len(entries))
	}
	versionID := entries[0].Name()
	got, err := os.ReadFile(filepath.Join(verDir, versionID))
	if err != nil {
		t.Fatalf("读取版本文件失败: %v", err)
	}
	if string(got) != string(body1) {
		t.Fatalf("版本文件内容 = %q, want %q（应为覆盖前内容）", got, body1)
	}

	// checksum key = "version/dir/f.txt/<id>"（per-tenant store，无 owner 前缀）
	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	wantCS := sha256hex(body1)
	csKey := "version/dir/f.txt/" + versionID
	if cs, ok := csStore.Get(csKey); !ok {
		t.Fatalf("checksum key %q 应存在", csKey)
	} else if cs != wantCS {
		t.Fatalf("checksum = %s, want %s", cs, wantCS)
	}
	// 旧 __version__ 前缀 key 不应存在（R4 碰撞消除）
	if _, ok := csStore.Get("__version__/dir/f.txt/" + versionID); ok {
		t.Fatal("旧 __version__ checksum key 不应存在")
	}
	// 用户文件本身落在 user 桶
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); err != nil {
		t.Fatalf("用户文件应落在 alice/user/dir/f.txt: %v", err)
	}
}

// ---- 直调领域方法的用例（不经 HTTP 路由）----

func TestSaveVersionBeforeOverwrite_InvalidPath(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Versioning.Enabled = true
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { _ = h.Close() })

	// 空路径 → UserRel 校验失败，记录 warn 并返回（不 panic）。
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/upload", nil)
	h.fileService().SaveVersionBeforeOverwrite(req, "", h.tenantOf(req))
}

// TestVersionHandlers_RejectTraversalVersionID 覆盖 version_id 未校验即拼路径的越界读/删回归：
// FindVersionFile 用 versionIDStr 拼 verRel（verDir + "/" + id），畸形 id 可越出
// version/<file>/ 子目录落到**同租户**的其它桶——DELETE 直接 Remove（**绕过 /delete 的
// checksum 门禁**），restore 把该文件拷回 user/ 桶（越界读）。修复后两者都走 404
// （与「版本不存在」同一条路径），且哨兵文件与 user 文件都不受影响。
//
// 断言口径：落**真实副作用**（哨兵文件仍存在且内容不变 / user 文件内容不变），不只断状态码。
func TestVersionHandlers_RejectTraversalVersionID(t *testing.T) {
	root := t.TempDir()
	baseURL, _, dirs := uploadingLockServer(t, "alice", singleVolumeLocks(root), func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 5
	})

	v1 := []byte("traversal version one")
	v2 := []byte("traversal version two (current)")
	if status, _, body := volumeUpload(t, baseURL, "trav.txt", v1, ""); status != http.StatusOK {
		t.Fatalf("首传应 200, got %d %s", status, body)
	}
	if status, _, body := volumeUpload(t, baseURL, "trav.txt", v2, ""); status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, body)
	}

	// 哨兵：同租户 meta 桶内的文件（畸形 version_id 拼出的落点）。
	tenantRoot := filepath.Join(dirs[0], "alice")
	if err := os.MkdirAll(filepath.Join(tenantRoot, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(tenantRoot, "meta", "sentinel.txt")
	const sentinelBody = "sentinel-must-survive"
	if err := os.WriteFile(sentinel, []byte(sentinelBody), 0o644); err != nil {
		t.Fatal(err)
	}

	const traversal = "../../meta/sentinel.txt"

	// DELETE：修复前直接 Remove 哨兵（绕过 /delete 的 checksum 门禁）。
	req, err := http.NewRequest(http.MethodDelete,
		baseURL+"/api/versions?filename=trav.txt&version_id="+traversal, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete version: %v", err)
	}
	delBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("越界 version_id 的 delete 应 404, got %d body=%s", resp.StatusCode, delBody)
	}
	if got, rerr := os.ReadFile(sentinel); rerr != nil || string(got) != sentinelBody {
		t.Fatalf("越界 version_id 不得删除 %s: err=%v content=%q（修复前该文件会被 Remove）", sentinel, rerr, got)
	}

	// restore：修复前把哨兵拷成 user/trav.txt（越界读）。
	status, body := postNoBody(t, baseURL+"/api/versions/restore?filename=trav.txt&version_id="+traversal)
	if status != http.StatusNotFound {
		t.Fatalf("越界 version_id 的 restore 应 404, got %d body=%s", status, body)
	}
	got, rerr := os.ReadFile(filepath.Join(tenantRoot, "user", "trav.txt"))
	if rerr != nil {
		t.Fatalf("读取 user 文件: %v", rerr)
	}
	if string(got) != string(v2) {
		t.Fatalf("越界 version_id 不得改写 user 文件: got %q want %q（修复前会被哨兵内容覆盖）", got, v2)
	}

	// 领域不变量 `version > 0`：非正 ID（历史 `UnixMilli*1000` 之前纳秒实现回绕的产物）是
	// **无效数据**，**既不列出也不可操作**。此处把负 ID 版本文件真的写到盘上，两侧一起验：
	// 操作侧必须 404 且不改写 user 文件；列表侧必须不出现该条目（正 ID 条目仍列出，作正向对照）。
	const legacyID = "-269429080180906331"
	if werr := os.WriteFile(filepath.Join(tenantRoot, "version", "trav.txt", legacyID), []byte("legacy"), 0o644); werr != nil {
		t.Fatal(werr)
	}

	status, body = postNoBody(t, baseURL+"/api/versions/restore?filename=trav.txt&version_id="+legacyID)
	if status != http.StatusNotFound {
		t.Fatalf("非正 version_id 的 restore 应 404（version > 0 是领域不变量）, got %d body=%s", status, body)
	}
	got, rerr = os.ReadFile(filepath.Join(tenantRoot, "user", "trav.txt"))
	if rerr != nil || string(got) != string(v2) {
		t.Fatalf("非正 version_id 不得改写 user 文件: got %q（err=%v）want %q", got, rerr, v2)
	}

	listResp, lerr := http.Get(baseURL + "/api/versions?filename=trav.txt")
	if lerr != nil {
		t.Fatalf("list versions: %v", lerr)
	}
	var listBody struct {
		Versions []VersionInfo `json:"versions"`
	}
	decErr := json.NewDecoder(listResp.Body).Decode(&listBody)
	listResp.Body.Close()
	if decErr != nil {
		t.Fatalf("解析 /api/versions 响应: %v", decErr)
	}
	// 正向对照：覆盖写产生了 1 个正 ID 版本（v1），列表不得因过滤过宽而变空。
	if len(listBody.Versions) == 0 {
		t.Fatal("正向对照失败：应至少列出 1 个正 ID 版本（覆盖写产生）")
	}
	// 负向：盘上那个负 ID 条目（含任何非正 ID）都不得出现——与操作侧同判据。
	for _, v := range listBody.Versions {
		if v.VersionID <= 0 {
			t.Fatalf("非正 ID 条目不应出现在列表中（version > 0 是领域不变量）, got version_id=%d", v.VersionID)
		}
	}
}

// TestDeleteVersion_NonCanonicalIDClearsChecksum 钉住 F37：删除版本时清理 checksum 的 key
// 必须与**版本文件的规范 rel**一致（FindVersionFile 回传的 verRel / 写侧 SaveVersion 的 key），
// **不得**用原始请求串拼 key。
//
// 场景：`version_id=%2B<id>`（URL 解码后 "+<id>"）——路径侧按生成值命中的是盘上规范名 `<id>`，
// 若 checksum 侧仍用原始串拼 key，则删除打空、真正写入的 `<id>` 条目成为**孤儿**。
// 断言两侧：① 版本文件**确实被删除**（修复没有把删除一起改坏）；② checksum 条目**不残留**。
func TestDeleteVersion_NonCanonicalIDClearsChecksum(t *testing.T) {
	root := t.TempDir()
	baseURL, h, dirs := uploadingLockServer(t, "alice", singleVolumeLocks(root), func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 5
	})

	v1 := []byte("f37 version one")
	v2 := []byte("f37 version two (current)")
	if status, _, body := volumeUpload(t, baseURL, "f37.txt", v1, ""); status != http.StatusOK {
		t.Fatalf("首传应 200, got %d %s", status, body)
	}
	if status, _, body := volumeUpload(t, baseURL, "f37.txt", v2, ""); status != http.StatusOK {
		t.Fatalf("覆盖写应 200, got %d %s", status, body)
	}

	verDir := filepath.Join(dirs[0], "alice", "version", "f37.txt")
	entries, lerr := os.ReadDir(verDir)
	if lerr != nil || len(entries) != 1 {
		t.Fatalf("版本目录应有 1 个版本: err=%v entries=%d", lerr, len(entries))
	}
	canonicalID := entries[0].Name()

	cs := h.checksumStoreFor("alice")
	if cs == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	csKey := "version/f37.txt/" + canonicalID
	if _, ok := cs.Get(csKey); !ok {
		t.Fatalf("前置条件失败：SaveVersion 应已写入 checksum key %q", csKey)
	}

	// 非规范拼写删除：%2B 解码为 "+"（客户端可构造，服务端 parseVersionID 接受 "+<id>"）。
	req, rerr := http.NewRequest(http.MethodDelete,
		baseURL+"/api/versions?filename=f37.txt&version_id=%2B"+canonicalID, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		t.Fatalf("delete version: %v", derr)
	}
	delBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("非规范拼写的 delete 应 200（路径侧按规范名命中）, got %d body=%s", resp.StatusCode, delBody)
	}

	// ① 文件确实被删除（删除能力未被改坏）。
	if _, serr := os.Stat(filepath.Join(verDir, canonicalID)); !os.IsNotExist(serr) {
		t.Fatalf("版本文件 %s 应已被删除, stat err=%v", canonicalID, serr)
	}
	// ② checksum 条目不残留（F37 前此处残留孤儿，keys 落在 "+<id>" 上）。
	if _, ok := cs.Get(csKey); ok {
		t.Fatalf("checksum 条目 %q 残留（孤儿）——删除用了原始请求串拼 key，未随路径规范化", csKey)
	}
}
