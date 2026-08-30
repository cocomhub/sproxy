// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFileClient_MakeDir(t *testing.T) {
	var gotDirname string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/mkdir" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		gotDirname = r.URL.Query().Get("dirname")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	if err := c.MakeDir(context.Background(), "sub/dir"); err != nil {
		t.Fatalf("MakeDir error: %v", err)
	}
	if gotDirname != "sub/dir" {
		t.Fatalf("dirname = %q, want %q", gotDirname, "sub/dir")
	}
}

func TestFileClient_MakeDir_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"success":false,"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	if err := c.MakeDir(context.Background(), "x"); err == nil {
		t.Fatalf("MakeDir 应返回错误")
	}
}
