// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetStats(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"disk_usage":{"uploads_dir":"./uploads","total_files":10,"total_size":1024},
			"request_counts":{"total":100,"2xx":80,"4xx":15,"5xx":5},
			"active_connections":3,
			"files_uploaded":5,"files_downloaded":20,"files_deleted":2,
			"bytes_uploaded":50000,"bytes_downloaded":200000,
			"max_storage_bytes":1073741824,"storage_usage":1048576,
			"disk_total":100000000000,"disk_free":50000000000,"disk_used":50000000000
		}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	stats, err := c.GetStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.DiskUsage.TotalFiles != 10 {
		t.Errorf("expected TotalFiles=10, got %d", stats.DiskUsage.TotalFiles)
	}
	if stats.RequestCounts.Total != 100 {
		t.Errorf("expected RequestCounts.Total=100, got %d", stats.RequestCounts.Total)
	}
	if stats.ActiveConns != 3 {
		t.Errorf("expected ActiveConns=3, got %d", stats.ActiveConns)
	}
}
