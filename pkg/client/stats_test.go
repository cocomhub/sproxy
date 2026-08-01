// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
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
			"storage_user_files":524288,"storage_chunked":262144,"storage_versions":131072,"storage_cloud":131072,
			"disk_total":100000000000,"disk_free":50000000000,"disk_used":50000000000
		}`))
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	stats, err := c.GetStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// DiskUsage (3 fields)
	if stats.DiskUsage.UploadsDir != "./uploads" {
		t.Errorf("expected UploadsDir=./uploads, got %q", stats.DiskUsage.UploadsDir)
	}
	if stats.DiskUsage.TotalFiles != 10 {
		t.Errorf("expected TotalFiles=10, got %d", stats.DiskUsage.TotalFiles)
	}
	if stats.DiskUsage.TotalSize != 1024 {
		t.Errorf("expected TotalSize=1024, got %d", stats.DiskUsage.TotalSize)
	}

	// RequestCounts (4 fields)
	if stats.RequestCounts.Total != 100 {
		t.Errorf("expected Total=100, got %d", stats.RequestCounts.Total)
	}
	if stats.RequestCounts.Status2xx != 80 {
		t.Errorf("expected Status2xx=80, got %d", stats.RequestCounts.Status2xx)
	}
	if stats.RequestCounts.Status4xx != 15 {
		t.Errorf("expected Status4xx=15, got %d", stats.RequestCounts.Status4xx)
	}
	if stats.RequestCounts.Status5xx != 5 {
		t.Errorf("expected Status5xx=5, got %d", stats.RequestCounts.Status5xx)
	}

	// Top-level fields (15 fields)
	if stats.ActiveConns != 3 {
		t.Errorf("expected ActiveConns=3, got %d", stats.ActiveConns)
	}
	if stats.FilesUploaded != 5 {
		t.Errorf("expected FilesUploaded=5, got %d", stats.FilesUploaded)
	}
	if stats.FilesDownloaded != 20 {
		t.Errorf("expected FilesDownloaded=20, got %d", stats.FilesDownloaded)
	}
	if stats.FilesDeleted != 2 {
		t.Errorf("expected FilesDeleted=2, got %d", stats.FilesDeleted)
	}
	if stats.BytesUploaded != 50000 {
		t.Errorf("expected BytesUploaded=50000, got %d", stats.BytesUploaded)
	}
	if stats.BytesDownloaded != 200000 {
		t.Errorf("expected BytesDownloaded=200000, got %d", stats.BytesDownloaded)
	}
	if stats.MaxStorageBytes != 1073741824 {
		t.Errorf("expected MaxStorageBytes=1073741824, got %d", stats.MaxStorageBytes)
	}
	if stats.StorageUsage != 1048576 {
		t.Errorf("expected StorageUsage=1048576, got %d", stats.StorageUsage)
	}
	if stats.StorageUserFiles != 524288 {
		t.Errorf("expected StorageUserFiles=524288, got %d", stats.StorageUserFiles)
	}
	if stats.StorageChunked != 262144 {
		t.Errorf("expected StorageChunked=262144, got %d", stats.StorageChunked)
	}
	if stats.StorageVersions != 131072 {
		t.Errorf("expected StorageVersions=131072, got %d", stats.StorageVersions)
	}
	if stats.StorageCloud != 131072 {
		t.Errorf("expected StorageCloud=131072, got %d", stats.StorageCloud)
	}
	if stats.DiskTotal != 100000000000 {
		t.Errorf("expected DiskTotal=100000000000, got %d", stats.DiskTotal)
	}
	if stats.DiskFree != 50000000000 {
		t.Errorf("expected DiskFree=50000000000, got %d", stats.DiskFree)
	}
	if stats.DiskUsed != 50000000000 {
		t.Errorf("expected DiskUsed=50000000000, got %d", stats.DiskUsed)
	}
}

func TestGetStats_ServerError(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	_, err := c.GetStats(t.Context())
	if err == nil {
		t.Fatal("expected error for server error")
	}
}
