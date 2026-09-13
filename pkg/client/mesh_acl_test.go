// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client_test

// mesh_acl_test.go 钉住 `GET /api/mesh/acl` 的客户端（W3：CLI `mesh acl` 用）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMeshACL_ParsesEntries(t *testing.T) {
	payload := map[string]any{
		"owner": "alice",
		"entries": []map[string]any{
			{"volume": "main", "node": "node-a", "fingerprint": "sha256:aa", "scope": "rw"},
			{"volume": "share", "node": "node-c", "fingerprint": "sha256:bb", "scope": "read"},
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mesh/acl" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer ts.Close()

	got, err := client.NewFileClient(ts.URL).MeshACL(t.Context())
	if err != nil {
		t.Fatalf("MeshACL: %v", err)
	}
	if got.Owner != "alice" || len(got.Entries) != 2 {
		t.Fatalf("解析不符: %+v", got)
	}
	if got.Entries[0].Volume != "main" || got.Entries[0].Node != "node-a" || got.Entries[0].Scope != "rw" {
		t.Errorf("第 1 条不符: %+v", got.Entries[0])
	}
	if got.Entries[1].Fingerprint != "sha256:bb" {
		t.Errorf("第 2 条不符: %+v", got.Entries[1])
	}
}

// TestMeshACL_EmptyEntries 钉住「无授权」是正常态：空数组 + 无错误（非 404/报错）。
func TestMeshACL_EmptyEntries(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner":"alice","entries":[]}`))
	}))
	defer ts.Close()

	got, err := client.NewFileClient(ts.URL).MeshACL(t.Context())
	if err != nil {
		t.Fatalf("空授权不应报错: %v", err)
	}
	if got.Owner != "alice" || len(got.Entries) != 0 {
		t.Fatalf("应为空列表: %+v", got)
	}
}

// TestMeshACL_Non200Errors 钉住非 200 报错（而非静默返回空结构）。
func TestMeshACL_Non200Errors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	if _, err := client.NewFileClient(ts.URL).MeshACL(t.Context()); err == nil {
		t.Fatal("401 应报错")
	}
}
