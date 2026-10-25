// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveURL_OK 验证 ResolveURL 调 GET /api/resolve?url= 并解码统一结构。
func TestResolveURL_OK(t *testing.T) {
	t.Parallel()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("url")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ResolveInfo{
			URL: gotQuery, Kind: "pikpak_share",
			Files: []ResolvedFile{{ID: "id-1", Name: "EXAMPLE-001A.mp4", Size: 4370580470, Type: "video/mp4"}},
		})
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	info, err := c.ResolveURL(t.Context(), "https://example.com/s/EXAMPLE-001")
	if err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}
	if gotPath != "/api/resolve" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery != "https://example.com/s/EXAMPLE-001" {
		t.Fatalf("url query = %q（必须原样传递）", gotQuery)
	}
	if info.Kind != "pikpak_share" || len(info.Files) != 1 {
		t.Fatalf("info = %+v", info)
	}
	if info.Files[0].Name != "EXAMPLE-001A.mp4" || info.Files[0].Size != 4370580470 {
		t.Fatalf("file = %+v", info.Files[0])
	}
}

// TestResolveURL_EmptyURL 验证空 URL fail-fast（不发请求）。
func TestResolveURL_EmptyURL(t *testing.T) {
	t.Parallel()
	c := NewFileClient("http://127.0.0.1:1")
	if _, err := c.ResolveURL(t.Context(), ""); err == nil {
		t.Fatal("空 URL 应报错")
	}
}

// TestResolveURL_ServerError 验证非 2xx 映射为错误（不静默返回空结构）。
func TestResolveURL_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"share not found"}`, http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewFileClient(srv.URL)
	if _, err := c.ResolveURL(t.Context(), "https://example.com/s/DEAD-BEEF"); err == nil {
		t.Fatal("502 应报错")
	}
}
