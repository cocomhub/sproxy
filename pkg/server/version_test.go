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

// ---- private method tests ----

func TestSaveVersionBeforeOverwrite_InvalidPath(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.UploadsDir = t.TempDir()
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
	h.saveVersionBeforeOverwrite(req, "")
}

func TestCleanupOldVersions_NoMaxVersions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	tnt := h.tenantFor("alice")
	if tnt == nil {
		t.Fatal("创建 alice 租户失败")
	}
	// MaxVersions 默认 0 → cleanup 直接返回，不报错。
	h.cleanupOldVersions("test.txt", tnt)
}
