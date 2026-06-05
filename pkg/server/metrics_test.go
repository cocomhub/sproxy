// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newTestServerWithMetrics 创建带 Metrics 的测试服务，并挂载 /metrics 路由。
func newTestServerWithMetrics(t *testing.T) (*httptest.Server, *Handlers) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(tmpDir, nil)
	m := NewMetrics()
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "test",
		buildAt:       "test",
		checksumStore: cs,
		logger:        slog.Default(),
		metrics:       m,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.upload)
	mux.HandleFunc("GET /download", h.download)
	mux.HandleFunc("POST /delete", h.delete)
	mux.HandleFunc("GET /metrics", h.MetricsHandler)

	ts := httptest.NewServer(h.metricsMiddleware(mux))
	t.Cleanup(ts.Close)
	return ts, h
}

func TestMetricsHandler_Empty(t *testing.T) {
	ts, _ := newTestServerWithMetrics(t)
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "sproxy_requests_total") {
		t.Errorf("response missing sproxy_requests_total:\n%s", body)
	}
}

func TestMetrics_RecordRequest(t *testing.T) {
	m := NewMetrics()
	m.RecordRequest(200)
	m.RecordRequest(201)
	m.RecordRequest(404)
	m.RecordRequest(500)

	if got := m.RequestsTotal.Load(); got != 4 {
		t.Errorf("RequestsTotal: want 4, got %d", got)
	}
	if got := m.Requests2xx.Load(); got != 2 {
		t.Errorf("Requests2xx: want 2, got %d", got)
	}
	if got := m.Requests4xx.Load(); got != 1 {
		t.Errorf("Requests4xx: want 1, got %d", got)
	}
	if got := m.Requests5xx.Load(); got != 1 {
		t.Errorf("Requests5xx: want 1, got %d", got)
	}
}

func TestMetrics_RecordUploadDownloadDelete(t *testing.T) {
	m := NewMetrics()
	m.RecordUpload(1024)
	m.RecordUpload(2048)
	m.RecordDownload(512)
	m.RecordDelete()

	if got := m.FilesUploaded.Load(); got != 2 {
		t.Errorf("FilesUploaded: want 2, got %d", got)
	}
	if got := m.BytesUploaded.Load(); got != 3072 {
		t.Errorf("BytesUploaded: want 3072, got %d", got)
	}
	if got := m.FilesDownloaded.Load(); got != 1 {
		t.Errorf("FilesDownloaded: want 1, got %d", got)
	}
	if got := m.BytesDownloaded.Load(); got != 512 {
		t.Errorf("BytesDownloaded: want 512, got %d", got)
	}
	if got := m.FilesDeleted.Load(); got != 1 {
		t.Errorf("FilesDeleted: want 1, got %d", got)
	}
}

func TestMetrics_ActiveConnections(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)

	// 发一个请求，metricsMiddleware 会在请求期间使 active +1，请求后归 0
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	resp.Body.Close()

	if got := h.metrics.ActiveConnections.Load(); got != 0 {
		t.Errorf("ActiveConnections after request: want 0, got %d", got)
	}
}

func TestMetricsMiddleware_CountsRequests(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)

	for i := range 3 {
		resp, err := http.Get(ts.URL + "/metrics")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	if got := h.metrics.RequestsTotal.Load(); got != 3 {
		t.Errorf("RequestsTotal: want 3, got %d", got)
	}
	if got := h.metrics.Requests2xx.Load(); got != 3 {
		t.Errorf("Requests2xx: want 3, got %d", got)
	}
}

func TestMetricsHandler_PrometheusFormat(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)
	h.metrics.RecordUpload(100)
	h.metrics.RecordDownload(200)
	h.metrics.RecordDelete()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	checks := []string{
		"# TYPE sproxy_requests_total counter",
		"sproxy_bytes_uploaded 100",
		"sproxy_bytes_downloaded 200",
		"sproxy_files_deleted 1",
		"# TYPE sproxy_active_connections gauge",
	}
	for _, c := range checks {
		if !strings.Contains(text, c) {
			t.Errorf("missing %q in metrics output:\n%s", c, text)
		}
	}
}
