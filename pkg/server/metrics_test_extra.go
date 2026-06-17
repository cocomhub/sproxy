// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMetricsHandler_MuxMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(tmpDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "test",
		buildAt:       "now",
		checksumStore: cs,
		metrics:       NewMetrics(),
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", h.MetricsHandler)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "sproxy_requests_total") {
		t.Errorf("expected prometheus output, got: %s", body)
	}
}

func TestMetricsHandler_NilMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	cs := NewChecksumStore(tmpDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := &Handlers{
		checksumStore: cs,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", h.MetricsHandler)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestBatchRenameHandler(t *testing.T) {
	// batchRename handler 不应 panic（即使输入无效）
	cfg := Default()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	RegisterRoutes(context.TODO(), mux, &cfgPtr, "test", "now", nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/batch-rename", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	// 只要不 panic 且返回非 5xx 即可
	if w.Code >= 500 {
		t.Errorf("unexpected 5xx: %d", w.Code)
	}
}
