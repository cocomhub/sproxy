// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
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
			"log_level":"info","log_format":"text","access_keys_set":true,
			"auth_token_set":true,"tunnel_key_set":false,
			"rate_limit_requests":10,"rate_limit_window":"1s",
			"max_storage_bytes":1073741824,"chunk_size":4194304,
			"upload_session_ttl":"24h0m0s",
			"versioning_enabled":false,"versioning_max_versions":5,
			"cloud_max_concurrent":3,"cloud_sync_threshold":20971520,
			"hub_enabled":false,"tls_enabled":true,
			"addr":":18083","storage_root":"./storage"
		}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	cfg, err := c.GetConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// 全部 17 个字段断言
	if cfg.LogLevel != "info" {
		t.Errorf("expected LogLevel=info, got %s", cfg.LogLevel)
	}
	if cfg.RateLimitRequests != 10 {
		t.Errorf("expected RateLimitRequests=10, got %d", cfg.RateLimitRequests)
	}
	if cfg.RateLimitWindow != "1s" {
		t.Errorf("expected RateLimitWindow=1s, got %s", cfg.RateLimitWindow)
	}
	if cfg.MaxStorageBytes != 1073741824 {
		t.Errorf("expected MaxStorageBytes=1073741824, got %d", cfg.MaxStorageBytes)
	}
	if cfg.ChunkSize != 4194304 {
		t.Errorf("expected ChunkSize=4194304, got %d", cfg.ChunkSize)
	}
	if cfg.UploadSessionTTL != "24h0m0s" {
		t.Errorf("expected UploadSessionTTL=24h0m0s, got %s", cfg.UploadSessionTTL)
	}
	if cfg.VersioningEnabled {
		t.Error("expected VersioningEnabled=false")
	}
	if cfg.VersioningMax != 5 {
		t.Errorf("expected VersioningMax=5, got %d", cfg.VersioningMax)
	}
	if cfg.CloudMaxConcurrent != 3 {
		t.Errorf("expected CloudMaxConcurrent=3, got %d", cfg.CloudMaxConcurrent)
	}
	if cfg.CloudSyncThreshold != 20971520 {
		t.Errorf("expected CloudSyncThreshold=20971520, got %d", cfg.CloudSyncThreshold)
	}
	if cfg.HubEnabled {
		t.Error("expected HubEnabled=false")
	}
	if !cfg.TLSEnabled {
		t.Error("expected TLSEnabled=true")
	}
	if cfg.Addr != ":18083" {
		t.Errorf("expected Addr=:18083, got %s", cfg.Addr)
	}
	if cfg.StorageRoot != "./storage" {
		t.Errorf("expected StorageRoot=./storage, got %s", cfg.StorageRoot)
	}
}

func TestGetConfig_ServerError(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	c := NewFileClient(ts.URL)
	_, err := c.GetConfig(t.Context())
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
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	err := c.UpdateConfig(t.Context(), map[string]any{
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
	t.Cleanup(ts.Close)
	c := NewFileClient(ts.URL)
	err := c.UpdateConfig(t.Context(), map[string]any{"log_level": "invalid"})
	if err == nil {
		t.Fatal("expected error for bad request")
	}
}
