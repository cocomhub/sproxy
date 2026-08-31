// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestCfgPtr 返回指向默认配置（uploadsDir=dir）的 atomic.Pointer[Config]，供 handler 测试写入 cfgPtr。
func newTestCfgPtr(dir string) *atomic.Pointer[Config] {
	var p atomic.Pointer[Config]
	cfg := Default()
	cfg.UploadsDir = dir
	p.Store(cfg)
	return &p
}

func setupCloudArchiveTestWithCfg(t *testing.T, modify func(*Config)) (*httptest.Server, *CloudDownloadManager, string) {
	t.Helper()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() {
		mgr.Close()
		os.RemoveAll(filepath.Join(dir, ".__cloud__"))
		os.RemoveAll(filepath.Join(dir, ".__downloads__"))
	})

	var cfgPtr atomic.Pointer[Config]
	serverCfg := Default()
	serverCfg.UploadsDir = dir
	if modify != nil {
		modify(serverCfg)
	}
	cfgPtr.Store(serverCfg)

	h := &Handlers{cloudMgr: mgr, logger: testLogger(), storageMgr: sm, cfgPtr: &cfgPtr, auditLogger: testLogger()}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
	mux.HandleFunc("POST /api/cloud/archive", h.cloudArchiveBatch)
	return httptest.NewServer(mux), mgr, dir
}

// setupCloudArchiveTest 创建归档 handler 测试所需的临时服务器、CloudDownloadManager 和临时目录。
func setupCloudArchiveTest(t *testing.T) (*httptest.Server, *CloudDownloadManager, string) {
	t.Helper()
	return setupCloudArchiveTestWithCfg(t, nil)
}

// createCompletedTask 创建一个已完成的任务，并在 __cloud__/<id>/ 下创建测试文件。
func createCompletedTask(t *testing.T, mgr *CloudDownloadManager, filename string) string {
	t.Helper()
	task, err := mgr.CreateTask("url", "https://example.com/"+filename, filename, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = "completed"
	cloudDir := filepath.Join(mgr.uploadsDir, cloudDirName)
	taskDir := filepath.Join(cloudDir, task.ID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, filename), []byte("test content"), 0644); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func TestCloudArchive_SingleTask(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	id := createCompletedTask(t, mgr, "test.zip")

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json", strings.NewReader(`{"archive_name":"single-task.tar.gz"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success=true, got message: %s", result.Message)
	}
	if result.File != "single-task.tar.gz" && !strings.HasSuffix(result.File, "/single-task.tar.gz") {
		t.Fatalf("expected file name %q, got %q", "single-task.tar.gz", result.File)
	}
	if result.Size <= 0 {
		t.Fatalf("expected positive size, got %d", result.Size)
	}
	if result.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
	if result.TaskCount != 1 {
		t.Fatalf("expected task_count=1, got %d", result.TaskCount)
	}
}

func TestCloudArchive_TaskNotFound(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/nonexistent/archive", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatal("expected success=false")
	}
	if result.Message != "task not found" {
		t.Fatalf("expected message %q, got %q", "task not found", result.Message)
	}
}

func TestCloudArchive_TaskNotCompleted(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	// 创建一个未完成的任务（默认 status = "pending"）
	task, err := mgr.CreateTask("url", "https://example.com/test.zip", "test.zip", 100, "")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/archive", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatal("expected success=false")
	}
	if !strings.Contains(result.Message, "completed") {
		t.Fatalf("expected message to contain 'completed', got %q", result.Message)
	}
}

func TestCloudArchive_BatchTasks(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	id1 := createCompletedTask(t, mgr, "file1.zip")
	id2 := createCompletedTask(t, mgr, "file2.zip")

	body := `{"task_ids":["` + id1 + `","` + id2 + `"],"archive_name":"batch-test.tar.gz"}`
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success=true, got message: %s", result.Message)
	}
	if result.File != "batch-test.tar.gz" && !strings.HasSuffix(result.File, "/batch-test.tar.gz") {
		t.Fatalf("expected file name %q, got %q", "batch-test.tar.gz", result.File)
	}
	if result.Size <= 0 {
		t.Fatalf("expected positive size, got %d", result.Size)
	}
	if result.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
	if result.TaskCount != 2 {
		t.Fatalf("expected task_count=2, got %d", result.TaskCount)
	}
}

func TestCloudArchive_BatchEmptyTaskIDs(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", strings.NewReader(`{"task_ids":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatal("expected success=false")
	}
	if result.Message != "task_ids is required" {
		t.Fatalf("expected message %q, got %q", "task_ids is required", result.Message)
	}
}

func TestCloudArchive_BatchTaskNotFound(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	id := createCompletedTask(t, mgr, "valid.zip")

	body := `{"task_ids":["` + id + `","nonexistent-id"]}`
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 新行为：跳过无效 task，继续处理有效 task，返回 200 部分成功
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success=true, got message: %s", result.Message)
	}
	if result.TaskCount != 1 {
		t.Fatalf("expected task_count=1, got %d", result.TaskCount)
	}
	if result.SkippedCount != 1 {
		t.Fatalf("expected skipped_count=1, got %d", result.SkippedCount)
	}
	if len(result.SkippedTasks) != 1 || result.SkippedTasks[0] != "nonexistent-id" {
		t.Fatalf("expected skipped_tasks=[\"nonexistent-id\"], got %v", result.SkippedTasks)
	}
}

func TestCloudArchive_BatchAllTasksSkipped(t *testing.T) {
	ts, _, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	body := `{"task_ids":["nonexistent-1","nonexistent-2"]}`
	resp, err := http.Post(ts.URL+"/api/cloud/archive", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Fatal("expected success=false")
	}
	if result.Message != "no valid tasks to archive" {
		t.Fatalf("expected message %q, got %q", "no valid tasks to archive", result.Message)
	}
	if result.SkippedCount != 2 {
		t.Fatalf("expected skipped_count=2, got %d", result.SkippedCount)
	}
}

func TestCloudArchive_DefaultArchiveName(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	id := createCompletedTask(t, mgr, "test.zip")

	// 不指定 archive_name，使用默认名称
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("expected success=true, got message: %s", result.Message)
	}
	if !strings.HasSuffix(result.File, ".tar.gz") {
		t.Fatalf("expected default archive name ending with '.tar.gz', got %q", result.File)
	}
	if result.Size <= 0 {
		t.Fatalf("expected positive size, got %d", result.Size)
	}
	if result.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
	if result.TaskCount != 1 {
		t.Fatalf("expected task_count=1, got %d", result.TaskCount)
	}
}

func TestCloudArchive_PreservesMTime(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	body := []byte("archive mtime test")
	uploadFile(t, url, "mtime-test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 获取原始文件 mtime
	info, _ := os.Stat(filepath.Join(cfgPtr.Load().UploadsDir, "mtime-test.txt"))
	originalMTime := info.ModTime()

	// 创建普通归档验证 mtime
	resp, err := http.Post(url+"/api/archive", "application/json",
		strings.NewReader(`{"files":["mtime-test.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gr, _ := gzip.NewReader(resp.Body)
	defer gr.Close()
	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}

	diff := header.ModTime.Sub(originalMTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("tar header ModTime %v differs from original %v (diff: %v)",
			header.ModTime, originalMTime, diff)
	}
}

// TestCloudArchive_SameNameConflict 验证同名归档已存在时返回 409（O_EXCL 防覆盖）。
func TestCloudArchive_SameNameConflict(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTest(t)
	defer ts.Close()

	id := createCompletedTask(t, mgr, "test.zip")

	// 第一次归档成功
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json",
		strings.NewReader(`{"archive_name":"conflict.tar.gz"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected first archive 200, got %d", resp.StatusCode)
	}

	// 同名再次归档 → 409
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json",
		strings.NewReader(`{"archive_name":"conflict.tar.gz"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on same-name archive, got %d", resp.StatusCode)
	}
}

// TestCloudArchive_SizeLimit 验证超过 cloud_archive_max_bytes 时返回 400。
func TestCloudArchive_SizeLimit(t *testing.T) {
	ts, mgr, _ := setupCloudArchiveTestWithCfg(t, func(c *Config) {
		c.CloudArchiveMaxBytes = 5 // 极小额限制，任何文件都超限
	})
	defer ts.Close()

	id := createCompletedTask(t, mgr, "test.zip")

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json",
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on size limit, got %d", resp.StatusCode)
	}

	var result CloudArchiveResult
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if result.Success {
		t.Fatal("expected success=false")
	}
	if !strings.Contains(result.Message, "cloud_archive_max_bytes") {
		t.Fatalf("expected message to mention cloud_archive_max_bytes, got %q", result.Message)
	}
}

// TestCloudArchive_QuotaRejected 验证存储配额不足时返回 507 且不泄漏预留。
func TestCloudArchive_QuotaRejected(t *testing.T) {
	ts, mgr, dir := setupCloudArchiveTest(t)
	defer ts.Close()

	id := createCompletedTask(t, mgr, "test.zip")

	// 占用配额至只剩少量余量（createCompletedTask 的 CreateTask 已预留 100 字节）：
	// 使归档预占（文件大小 + 100MB）必然超限。预留 MaxBytes()-1100 后剩余 ~1000 字节，
	// 归档预占失败后这 1000 字节仍可用——验证 507 时账本未被拖高（预占被正确释放）。
	sm := mgr.storage
	reserved := sm.MaxBytes() - 1100
	if err := sm.TryReserve(reserved, CategoryCloud); err != nil {
		t.Fatal(err)
	}
	defer sm.Release(reserved, CategoryCloud)

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", "application/json",
		strings.NewReader(`{"archive_name":"quota.tar.gz"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 on quota, got %d", resp.StatusCode)
	}

	// 预留未泄漏：507 后仍可预留 100 字节（若预占泄漏，此处会再超限）
	if err := sm.TryReserve(100, CategoryCloud); err != nil {
		t.Fatalf("expected ledger not leaked after 507, got: %v", err)
	}
	sm.Release(100, CategoryCloud)
	_ = dir
}
