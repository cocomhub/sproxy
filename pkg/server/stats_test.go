// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestStats_Empty(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var stats StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.DiskUsage.TotalFiles != 0 {
		t.Fatalf("expected 0 files, got %d", stats.DiskUsage.TotalFiles)
	}
}

func TestStats_AfterUpload(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("hello stats")
	uploadFile(t, url, "stats-test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var stats StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.DiskUsage.TotalFiles != 1 {
		t.Fatalf("expected 1 file, got %d", stats.DiskUsage.TotalFiles)
	}
	if stats.DiskUsage.TotalSize != int64(len(body)) {
		t.Fatalf("expected size %d, got %d", len(body), stats.DiskUsage.TotalSize)
	}
}

func TestStats_Fields(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}

	// 验证顶层字段存在
	for _, field := range []string{"disk_usage", "request_counts", "active_connections", "files_uploaded", "bytes_uploaded"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing field: %s", field)
		}
	}
}
