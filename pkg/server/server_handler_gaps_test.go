// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

func TestTunnelHandler_ReturnsHandler(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()
	th := h.TunnelHandler()
	if th == nil {
		t.Fatal("TunnelHandler() returned nil")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/tunnel", nil)
	th.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
		t.Errorf("expected 400 or 403 for invalid tunnel frame, got %d", w.Code)
	}
}

func TestHandler_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()
	handler := h.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestHandler_HealthzRoute(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("expected body 'OK', got '%s'", string(body))
	}
}

func TestHandler_VersionRoute(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "v1.0.0", "2026-06-13", nil, testLogger(), nil)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /version: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("expected non-empty version body")
	}
}

func TestHandler_UploadRouteRequiresAuth(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.AuthToken = "secret"
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/upload", "multipart/form-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated upload, got %d", resp.StatusCode)
	}
}

func TestUpdateKey(t *testing.T) {
	t.Parallel()

	key1Hex := testKey()
	key1, err := tunnel.ParseKey(key1Hex)
	if err != nil {
		t.Fatal(err)
	}
	key2Hex := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	key2, err := tunnel.ParseKey(key2Hex)
	if err != nil {
		t.Fatal(err)
	}

	tunnelLogger := testLogger()
	th := tunnel.NewLocalHandler(key1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}), tunnelLogger)

	srv := httptest.NewServer(th)
	defer srv.Close()

	client1, err := tunnel.NewClient(key1Hex, srv.URL, time.Second, tunnelLogger)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	resp, err := client1.Do(req)
	if err != nil {
		t.Fatalf("key1 request failed: %v", err)
	}
	resp.Body.Close()

	if updater, ok := th.(*tunnel.Handler); ok {
		updater.UpdateKey(key2)
	} else {
		t.Fatal("tunnel handler does not implement UpdateKey")
	}

	_, err = client1.Do(req)
	if err == nil {
		t.Error("expected error after key update with old key, got nil")
	}

	client2, err := tunnel.NewClient(key2Hex, srv.URL, time.Second, tunnelLogger)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client2.Do(req)
	if err != nil {
		t.Fatalf("key2 request failed: %v", err)
	}
	resp.Body.Close()
}
