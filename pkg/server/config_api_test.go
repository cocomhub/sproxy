// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestConfig_GetConfig(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var cfg configResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.LogLevel != "error" {
		t.Errorf("expected log_level=error (test default), got %s", cfg.LogLevel)
	}
	if cfg.RateLimitRequests != 10 {
		t.Errorf("expected rate_limit_requests=10, got %d", cfg.RateLimitRequests)
	}
	if cfg.ChunkSize <= 0 {
		t.Errorf("expected chunk_size > 0, got %d", cfg.ChunkSize)
	}
}

func TestConfig_UpdateLogLevel(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	body := strings.NewReader(`{"log_level":"debug"}`)
	req, err := http.NewRequest("PUT", url+"/api/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log_level=debug, got %s", cfg.LogLevel)
	}
}

func TestConfig_UpdateLogFormat(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"log_format":"json"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.LogFormat != "json" {
		t.Errorf("expected log_format=json, got %s", cfg.LogFormat)
	}
}

func TestConfig_UpdateAuthToken(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"auth_token":"new-token-123"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.AuthToken != "new-token-123" {
		t.Errorf("expected auth_token=new-token-123, got %s", cfg.AuthToken)
	}
}

func TestConfig_UpdateMaxStorageBytes(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"max_storage_bytes":104857600}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.MaxStorageBytes != 104857600 {
		t.Errorf("expected max_storage_bytes=104857600, got %d", cfg.MaxStorageBytes)
	}
}

func TestConfig_UpdateRateLimit(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"rate_limit_requests":20,"rate_limit_window":"5s"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.RateLimit.Requests != 20 {
		t.Errorf("expected rate_limit_requests=20, got %d", cfg.RateLimit.Requests)
	}
	if cfg.RateLimit.Window.String() != "5s" {
		t.Errorf("expected rate_limit_window=5s, got %s", cfg.RateLimit.Window)
	}
}

func TestConfig_UpdateInvalidInput(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"invalid log_level", `{"log_level":"invalid"}`, http.StatusBadRequest},
		{"invalid log_format", `{"log_format":"xml"}`, http.StatusBadRequest},
		{"negative rate_limit", `{"rate_limit_requests":-1}`, http.StatusBadRequest},
		{"invalid rate_window", `{"rate_limit_window":"-1s"}`, http.StatusBadRequest},
		{"negative max_storage", `{"max_storage_bytes":-1}`, http.StatusBadRequest},
		{"malformed json", `{bad json}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

func TestConfig_UpdateEmptyBody(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	if result["success"] != true {
		t.Error("expected success=true")
	}
	if result["changed"] != false {
		t.Error("expected changed=false for empty body")
	}
}
