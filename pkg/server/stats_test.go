// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
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

func TestStats_StorageFields(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024 // 100 MiB
	})

	resp, err := http.Get(url + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}

	// 验证存储字段存在
	for _, field := range []string{"max_storage_bytes", "storage_usage", "storage_user_files", "storage_chunked", "storage_versions", "storage_cloud"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing storage field: %s", field)
		}
	}

	// 验证 max_storage_bytes 与配置一致
	maxBytes, ok := raw["max_storage_bytes"].(float64)
	if !ok || int64(maxBytes) != 100*1024*1024 {
		t.Errorf("expected max_storage_bytes=%d, got %v", 100*1024*1024, raw["max_storage_bytes"])
	}

	// 验证 disk_total/disk_free 存在（值取决于实际文件系统）
	for _, field := range []string{"disk_total", "disk_free", "disk_used"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("missing disk stat field: %s", field)
		}
	}
}

func TestStorageConfig_Put(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024 // 100 MiB
	})

	// 请求体
	body := bytes.NewReader([]byte(`{"max_storage_bytes": 21474836480}`))
	req, err := http.NewRequest(http.MethodPut, url+"/api/storage/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}

	// 验证响应包含更新后的值
	got, ok := raw["max_storage_bytes"].(float64)
	if !ok || int64(got) != 21474836480 {
		t.Errorf("expected max_storage_bytes=21474836480, got %v", raw["max_storage_bytes"])
	}

	// 验证 storageMgr 上限已更新
	cfg := cfgPtr.Load()
	if cfg.MaxStorageBytes != 21474836480 {
		t.Errorf("expected config.MaxStorageBytes=21474836480, got %d", cfg.MaxStorageBytes)
	}
}

func TestStorageConfig_Put_BadRequest(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 无效请求体
	body := bytes.NewReader([]byte(`invalid json`))
	req, _ := http.NewRequest(http.MethodPut, url+"/api/storage/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStorageConfig_Put_NegativeValue(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024
	})

	body := bytes.NewReader([]byte(`{"max_storage_bytes": -1}`))
	req, _ := http.NewRequest(http.MethodPut, url+"/api/storage/config", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative value, got %d", resp.StatusCode)
	}
}
