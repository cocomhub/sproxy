// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	if err := json.NewDecoder(resp.Body).Decode(&listResult); err != nil {
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
