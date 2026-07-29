// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientListVersions(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/versions" {
			t.Errorf("expected /api/versions, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("filename") != "test.txt" {
			t.Errorf("expected filename=test.txt, got %s", r.URL.Query().Get("filename"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"versions": []VersionInfo{
				{Filename: "test.txt", VersionID: 1, Size: 100, Checksum: "abc123", CreatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)},
				{Filename: "test.txt", VersionID: 2, Size: 200, Checksum: "def456", CreatedAt: time.Now().Format(time.RFC3339)},
			},
		})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	versions, err := c.ListVersions(t.Context(), "test.txt")
	if err != nil {
		t.Fatalf("ListVersions() = %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("expected 2 versions, got %d", len(versions))
	}
	if versions[0].VersionID != 1 {
		t.Errorf("versions[0].VersionID = %d, want 1", versions[0].VersionID)
	}
	if versions[0].Checksum != "abc123" {
		t.Errorf("versions[0].Checksum = %q, want abc123", versions[0].Checksum)
	}
}

func TestClientListVersions_NotFound(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"Success": false, "Message": "file not found"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	_, err := c.ListVersions(t.Context(), "nonexistent.txt")
	if err == nil {
		t.Error("expected error for 404, got nil")
	}
}

func TestClientRestoreVersion(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Query().Get("filename") != "test.txt" {
			t.Errorf("expected filename=test.txt, got %s", r.URL.Query().Get("filename"))
		}
		if r.URL.Query().Get("version_id") != "1" {
			t.Errorf("expected version_id=1, got %s", r.URL.Query().Get("version_id"))
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"Success": true, "Message": "restored"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.RestoreVersion(t.Context(), "test.txt", 1)
	if err != nil {
		t.Fatalf("RestoreVersion() = %v", err)
	}
}

func TestClientRestoreVersion_Failure(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"Success": false, "Message": "version not found"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.RestoreVersion(t.Context(), "test.txt", 999)
	if err == nil {
		t.Error("expected error for failed restore, got nil")
	}
}

func TestClientDeleteVersion(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/api/versions" {
			t.Errorf("expected /api/versions, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("filename") != "test.txt" {
			t.Errorf("expected filename=test.txt, got %s", r.URL.Query().Get("filename"))
		}
		if r.URL.Query().Get("version_id") != "1" {
			t.Errorf("expected version_id=1, got %s", r.URL.Query().Get("version_id"))
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"Success": true, "Message": "deleted"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.DeleteVersion(t.Context(), "test.txt", 1)
	if err != nil {
		t.Fatalf("DeleteVersion() = %v", err)
	}
}

func TestClientDeleteVersion_Failure(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"Success": false, "Message": "version not found"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.DeleteVersion(t.Context(), "test.txt", 999)
	if err == nil {
		t.Error("expected error for failed delete, got nil")
	}
}

// TestClientVersionID_Int64Boundary 测试 VersionID 的 int64 边界值。
func TestClientVersionID_Int64Boundary(t *testing.T) {
	// VersionID 在 JSON 中是 int64，验证边界值能被正确解析
	validJSON := `{"versions":[{"filename":"test.txt","version_id":9223372036854775807,"size":100,"created_at":"2026-01-01T00:00:00Z"}]}`
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validJSON))
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	versions, err := c.ListVersions(t.Context(), "test.txt")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	if versions[0].VersionID != 9223372036854775807 {
		t.Errorf("expected VersionID 9223372036854775807, got %d", versions[0].VersionID)
	}
}

// TestClientVersionID_Zero 测试 VersionID 为 0 的场景。
func TestClientVersionID_Zero(t *testing.T) {
	validJSON := `{"versions":[{"filename":"test.txt","version_id":0,"size":0,"created_at":"2026-01-01T00:00:00Z"}]}`
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(validJSON))
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	versions, err := c.ListVersions(t.Context(), "test.txt")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(versions))
	}
	if versions[0].VersionID != 0 {
		t.Errorf("expected VersionID 0, got %d", versions[0].VersionID)
	}
}
