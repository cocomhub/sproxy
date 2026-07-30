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

func TestGetConfig(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"log_level":"info","log_format":"text",
			"auth_token_set":true,"tunnel_key_set":false,
			"rate_limit_requests":10,"rate_limit_window":"1s",
			"max_storage_bytes":0,"chunk_size":4194304,
			"upload_session_ttl":"24h0m0s",
			"versioning_enabled":false,"versioning_max_versions":0,
			"cloud_max_concurrent":3,"cloud_sync_threshold":20971520,
			"hub_enabled":false,"tls_enabled":true,
			"addr":":18083","uploads_dir":"./uploads"
		}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	cfg, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected LogLevel=info, got %s", cfg.LogLevel)
	}
	if !cfg.AuthTokenSet {
		t.Error("expected AuthTokenSet=true")
	}
	if cfg.RateLimitRequests != 10 {
		t.Errorf("expected RateLimitRequests=10, got %d", cfg.RateLimitRequests)
	}
}

func TestGetConfig_ServerError(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	c := NewFileClient(ts.URL)
	_, err := c.GetConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestUpdateConfig(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// 解析请求体并验证 log_level 字段
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body["log_level"] != "debug" {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"changed":true}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	err := c.UpdateConfig(context.Background(), map[string]any{
		"log_level": "debug",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateConfig_ServerError(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()
	c := NewFileClient(ts.URL)
	err := c.UpdateConfig(context.Background(), map[string]any{"log_level": "invalid"})
	if err == nil {
		t.Fatal("expected error for bad request")
	}
}
