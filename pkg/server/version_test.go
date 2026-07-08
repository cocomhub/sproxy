// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

// ---- private method tests ----

func TestSaveVersionBeforeOverwrite_InvalidPath(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.UploadsDir = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	key := make([]byte, 32)
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:       mux,
		CfgPtr:    &cfgPtr,
		Version:   "test",
		BuildAt:   "test",
		TunnelKey: key,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { _ = h.Close() })

	h.saveVersionBeforeOverwrite("")
}

func TestCleanupOldVersions_NoMaxVersions(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.UploadsDir = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	key := make([]byte, 32)
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:       mux,
		CfgPtr:    &cfgPtr,
		Version:   "test",
		BuildAt:   "test",
		TunnelKey: key,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { _ = h.Close() })

	h.cleanupOldVersions("test.txt", t.TempDir())
}
