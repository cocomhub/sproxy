// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
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

// TestStats_SkipsInternalDirsAtAnyDepth 验证（审查 #5）：stats 遍历跳过任意层级出现的服务端
// 内部目录——多租户 owner 隔离后版本目录出现在 owner 子目录下（uploadsDir/<owner>/.__versions__/），
// 若只按名字/仅根层跳过会把历史版本文件计为用户文件，导致 TotalFiles/TotalSize 虚高。
// 这里直接在当前 owner 存储根内预置 .__versions__ 子目录结构，然后组装 Handlers 调 WalkDir
// 并断言跳过（不依赖 storageMgr 首次扫描，避免脆性）。
func TestStats_SkipsInternalDirsAtAnyDepth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 布局：owner 根下用户文件 + owner 子目录下的内部版本目录 + 深层含 .__ 的普通文件
	ownerRoot := filepath.Join(dir, "ak-A")
	if err := os.MkdirAll(filepath.Join(ownerRoot, ".__versions__", "doc.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(ownerRoot, "user.txt")
	if err := os.WriteFile(userFile, []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, ".__versions__", "doc.txt", "123"), []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}
	// 深层含 .__ 的普通文件仍统计（非内部目录首段）：sub/foo.__bar.txt
	if err := os.MkdirAll(filepath.Join(ownerRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, "sub", "foo.__bar.txt"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: dir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}

	// 复用 statsHandler 的 WalkDir 逻辑：以空 owner 全局视角统计，验证内部目录被跳过
	totalFiles, totalSize := h.walkUploadStats(dir)
	if totalFiles != 2 { // user.txt + sub/foo.__bar.txt
		t.Fatalf("全局统计文件数 = %d, want 2（用户文件 + 深层 .__ 普通文件）", totalFiles)
	}
	if totalSize != int64(len("user")+len("y")) {
		t.Fatalf("全局统计大小 = %d, want %d", totalSize, int64(len("user")+len("y")))
	}
}

// TestStats_OwnerScopedCategories 验证（审查 M5 收敛）：认证用户视角的分类用量只含
// 自己 owner 根下的文件——owner 根下 .__chunked__/.__versions__ 归对应分类，
// 全局 .__cloud__（他人/全局云任务文件）不计入认证用户视角（云分类为 0）。
func TestStats_OwnerScopedCategories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ownerRoot := filepath.Join(dir, "ak-A")
	if err := os.MkdirAll(filepath.Join(ownerRoot, ".__versions__", "doc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ownerRoot, ".__chunked__"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, "user.txt"), []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, ".__versions__", "doc", "v1"), []byte("ver"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerRoot, ".__chunked__", "chunk.dat"), []byte("chunk"), 0644); err != nil {
		t.Fatal(err)
	}
	// 全局 .__cloud__（他人/全局云任务文件）不属于认证用户视角
	if err := os.MkdirAll(filepath.Join(dir, ".__cloud__", "t1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".__cloud__", "t1", "a.bin"), []byte("clouddata"), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg atomic.Pointer[Config]
	cfg.Store(&Config{UploadsDir: dir})
	h := &Handlers{cfgPtr: &cfg, logger: testLogger()}

	userFiles, chunked, versions, cloud := h.walkUploadStatsByCategory(ownerRoot)
	// user.txt=4, versions=3, chunked=5, cloud=0（owner 根下无全局 .__cloud__）
	if userFiles != 4 {
		t.Fatalf("userFiles = %d, want 4", userFiles)
	}
	if versions != 3 {
		t.Fatalf("versions = %d, want 3", versions)
	}
	if chunked != 5 {
		t.Fatalf("chunked = %d, want 5", chunked)
	}
	if cloud != 0 {
		t.Fatalf("cloud = %d, want 0（owner 根下不应含全局云任务）", cloud)
	}
}

func TestStorageConfig_Put(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.MaxStorageBytes = 100 * 1024 * 1024 // 100 MiB
	})

	// 请求体
	body := bytes.NewReader([]byte(`{"max_storage_bytes": 21474836480}`))
	req, err := http.NewRequest(http.MethodPut, url+"/api/config", body)
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
	if raw["success"] != true || raw["changed"] != true {
		t.Errorf("expected success=true, changed=true, got success=%v changed=%v", raw["success"], raw["changed"])
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
	req, _ := http.NewRequest(http.MethodPut, url+"/api/config", body)
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
	req, _ := http.NewRequest(http.MethodPut, url+"/api/config", body)
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
