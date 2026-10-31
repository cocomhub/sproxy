// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/compressx"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// newTestCfgPtr 返回指向默认配置（uploadsDir=dir）的 atomic.Pointer[Config]，供 handler 测试写入 cfgPtr。
func newTestCfgPtr(dir string) *atomic.Pointer[Config] {
	var p atomic.Pointer[Config]
	cfg := Default()
	cfg.StorageRoot = dir
	p.Store(cfg)
	return &p
}

func setupCloudArchiveTestWithCfg(t *testing.T, modify func(*Config)) (*httptest.Server, *cloud.CloudDownloadManager, string) {
	t.Helper()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	h := newAssemblyTestHandlers(t, dir)
	h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{UploadsDir: dir, Storage: cloudStorageManager{m: sm}, TenantFor: h.tenantFor, ChecksumStoreFor: h.checksumStoreFor, ListTenants: h.listTenantIDs, Logger: testLogger(), Config: cfg})
	h.cloudMgr = mgr

	serverCfg := h.cfgPtr.Load()
	if modify != nil {
		modify(serverCfg)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.archiveTaskHandler())
	mux.HandleFunc("POST /api/cloud/archive", h.archiveBatchHandler())
	return httptest.NewServer(mux), mgr, dir
}

// setupCloudArchiveTest 创建归档 handler 测试所需的临时服务器、cloud.CloudDownloadManager 和临时目录。
func setupCloudArchiveTest(t *testing.T) (*httptest.Server, *cloud.CloudDownloadManager, string) {
	t.Helper()
	return setupCloudArchiveTestWithCfg(t, nil)
}

// createCompletedTask 创建一个已完成的任务，并在 <tenant>/cloud/<id>/ 下创建测试文件。
func createCompletedTask(t *testing.T, mgr *cloud.CloudDownloadManager, filename string) string {
	t.Helper()
	task, err := mgr.CreateTask("url", "https://example.com/"+filename, filename, 100, "", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}
	task.Status = "completed"
	taskDir := filepath.Join(mgr.CloudDirFor(""), task.ID)
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

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", contentTypeJSON, strings.NewReader(`{"archive_name":"single-task.tar.gz"}`))
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

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/nonexistent/archive", contentTypeJSON, strings.NewReader(`{}`))
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
	task, err := mgr.CreateTask("url", "https://example.com/test.zip", "test.zip", 100, "", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/archive", contentTypeJSON, strings.NewReader(`{}`))
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
	resp, err := http.Post(ts.URL+"/api/cloud/archive", contentTypeJSON, strings.NewReader(body))
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

	resp, err := http.Post(ts.URL+"/api/cloud/archive", contentTypeJSON, strings.NewReader(`{"task_ids":[]}`))
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
	resp, err := http.Post(ts.URL+"/api/cloud/archive", contentTypeJSON, strings.NewReader(body))
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
	resp, err := http.Post(ts.URL+"/api/cloud/archive", contentTypeJSON, strings.NewReader(body))
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
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", contentTypeJSON, strings.NewReader(`{}`))
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
	if !strings.HasSuffix(result.File, tarGZExt) {
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

	// 获取原始文件 mtime（未认证上传落 <storageRoot>/anonymous/user/）
	info, err := os.Stat(filepath.Join(cfgPtr.Load().StorageRoot, anonymousOwner, "user", "mtime-test.txt"))
	if err != nil {
		t.Fatalf("stat uploaded file: %v", err)
	}
	originalMTime := info.ModTime()

	// 创建普通归档验证 mtime
	resp, err := http.Post(url+"/api/archive", contentTypeJSON,
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
	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", contentTypeJSON,
		strings.NewReader(`{"archive_name":"conflict.tar.gz"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected first archive 200, got %d", resp.StatusCode)
	}

	// 同名再次归档 → 409
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", contentTypeJSON,
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

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+id+"/archive", contentTypeJSON,
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
// P5 收敛后归档配额走租户 archive 桶 Scope（storageMgr 仅回退），故用租户上限触发 507。
func TestCloudArchive_QuotaRejected(t *testing.T) {
	env := newOwnerEnv(t)
	// 租户上限 1100：CreateTask 已预留 100（cloud 桶），归档预占（源 + 100MB 占位）必然超限。
	env.setOwnerQuota("alice", 1100)
	sm := capacity.NewStorageManager(env.root, 1024*1024, nil, testLogger())
	env.h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{UploadsDir: env.root, Storage: cloudStorageManager{m: sm}, TenantFor: env.h.tenantFor, ChecksumStoreFor: env.h.checksumStoreFor, ListTenants: env.h.listTenantIDs, Logger: testLogger(), Config: &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}, QuotaFor: []cloud.QuotaResolver{func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	}}})
	env.h.cloudMgr = mgr

	// 创建已完成云任务 + 落盘文件（新布局 <root>/alice/cloud/<id>/<file>）
	task, err := mgr.CreateTask("url", "https://example.com/quota.zip", "quota.zip", 100, "alice", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = "completed"
	taskDir := filepath.Join(mgr.CloudDirFor("alice"), task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "quota.zip"), []byte("quota data"), 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}

	aliceMux := env.mux["alice"]
	aliceMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.archiveTaskHandler()(w, r)
	})
	req := httptest.NewRequest("POST", "/api/cloud/tasks/"+task.ID+"/archive", strings.NewReader(`{"archive_name":"quota.tar.gz"}`))
	req.Header.Set(headerContentType, contentTypeJSON)
	rr := httptest.NewRecorder()
	aliceMux.ServeHTTP(rr, req)
	if rr.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 on quota, got %d body=%s", rr.Code, rr.Body.String())
	}

	// 预留未泄漏（任务 7 新语义：创建期不再在 Scope 占位——已知大小走写盘 QuotaWriter 边写边记，
	// 507 后无任何占用）：归档预占已回滚、云桶无残留。
	if got := env.h.quotaBucketFor("alice", "cloud").Reserved(); got != 0 {
		t.Fatalf("507 后 cloud 桶 Reserved()=%d want 0（创建期无占位，归档预占已回滚）", got)
	}
	if got := env.h.quotaBucketFor("alice", "cloud").Usage(); got != 0 {
		t.Fatalf("507 后 cloud 桶 Usage()=%d want 0", got)
	}
}

// TestCloudArchive_Delete_FileAlreadyGoneReleasesFromRegistry 验证（审查 D）：
// 归档文件已被外部手工删除（磁盘不存在、size<=0）时，deleteCloudArchive 回退按
// recordArchiveUsage 登记的字节释放 archive 桶 Scope（cloud_archive_handler.go 的
// size<=0 分支），归档桶不虚高。若删除只按磁盘 stat 释放，文件已消失时 stat 为 0
// 将跳过 ReleaseUsage → archive 桶 committed 残留（虚高），下次周期扫描前一直失守。
func TestCloudArchive_Delete_FileAlreadyGoneReleasesFromRegistry(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 1<<30)
	sm := capacity.NewStorageManager(env.root, 10*1024*1024*1024, nil, testLogger())
	env.h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{UploadsDir: env.root, Storage: cloudStorageManager{m: sm}, TenantFor: env.h.tenantFor, ChecksumStoreFor: env.h.checksumStoreFor, ListTenants: env.h.listTenantIDs, Logger: testLogger(), Config: &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}, QuotaFor: []cloud.QuotaResolver{func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	}}})
	env.h.cloudMgr = mgr

	// 创建已完成云任务 + 落盘文件
	task, err := mgr.CreateTask("url", "https://example.com/gone.zip", "gone.zip", 100, "alice", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = "completed"
	taskDir := filepath.Join(mgr.CloudDirFor("alice"), task.ID)
	if mkErr := os.MkdirAll(taskDir, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(taskDir, "gone.zip"), []byte("gone archive data"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	// alice mux 追加归档 handler（注入 alice actor ctx）
	aliceMux := env.mux["alice"]
	aliceMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.archiveTaskHandler()(w, r)
	})
	req := httptest.NewRequest("POST", "/api/cloud/tasks/"+task.ID+"/archive", strings.NewReader(`{"archive_name":"gone.tar.gz"}`))
	req.Header.Set(headerContentType, contentTypeJSON)
	rr := httptest.NewRecorder()
	aliceMux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("创建归档应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	archiveAbs := filepath.Join(env.root, "alice", "archive", "gone.tar.gz")
	info, err := os.Stat(archiveAbs)
	if err != nil {
		t.Fatalf("归档未落盘: %v", err)
	}
	archiveScope := env.h.quotaBucketFor("alice", "archive")
	regSize := info.Size()
	if got := archiveScope.Usage(); got != regSize {
		t.Fatalf("归档后 archive 桶 Usage()=%d want %d（=登记大小）", got, regSize)
	}

	// 手工删除归档物理文件（模拟外部清理/崩溃后文件已消失），登记仍在。
	if err := os.Remove(archiveAbs); err != nil {
		t.Fatalf("手工删除归档: %v", err)
	}
	if _, err := os.Stat(archiveAbs); !os.IsNotExist(err) {
		t.Fatalf("归档文件应已删除, stat err=%v", err)
	}

	// deleteCloudArchive：文件已不存在（stat size=0）→ 按登记释放 archive 桶。
	if err := env.h.deleteCloudArchive("alice", "gone.tar.gz"); err != nil {
		t.Fatalf("deleteCloudArchive: %v", err)
	}
	if got := archiveScope.Usage(); got != 0 {
		t.Fatalf("文件已不在后删除归档，archive 桶 Usage()=%d want 0（按登记释放，不虚高）", got)
	}
	// 登记已清除。
	env.h.tenantMu.Lock()
	_, stillReg := env.h.archiveUsage["alice"]["gone.tar.gz"]
	env.h.tenantMu.Unlock()
	if stillReg {
		t.Fatal("删除后登记应已清除")
	}
	// 幂等：再次删除（登记也不存在）仍成功且桶不虚高。
	if err := env.h.deleteCloudArchive("alice", "gone.tar.gz"); err != nil {
		t.Fatalf("再次 deleteCloudArchive 应幂等成功: %v", err)
	}
	if got := archiveScope.Usage(); got != 0 {
		t.Fatalf("再次删除后 archive 桶 Usage()=%d want 0", got)
	}
}

// TestCloudArchive_NewLayout 验证云归档迁移到租户 archive 桶：
//   - 归档落 <root>/alice/archive/<name>.tar.gz（不再落 .__cloud_archives__/）
//   - 同名预置在 alice/archive/ → 409（冲突检查落 archive 桶，O_EXCL 语义保留）
//   - kind=cloud_archive 下载 filename=<name> → 200（FeatureRel("archive", name) 解析）
func TestCloudArchive_NewLayout(t *testing.T) {
	env := newOwnerEnv(t) // alice 下载环境（含 <root>/alice/... 布局与 GET /download）
	root := env.root

	// 装配 cloudMgr + storageMgr（cloudArchiveTask 依赖任务快照与配额对账）
	sm := capacity.NewStorageManager(root, 10*1024*1024*1024, nil, testLogger())
	env.h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{UploadsDir: root, Storage: cloudStorageManager{m: sm}, TenantFor: env.h.tenantFor, ChecksumStoreFor: env.h.checksumStoreFor, ListTenants: env.h.listTenantIDs, Logger: testLogger(), Config: &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}})
	env.h.cloudMgr = mgr

	// 创建已完成云任务 + 落盘文件（新布局 <root>/alice/cloud/<id>/<file>）
	task, err := mgr.CreateTask("url", "https://example.com/newlayout.zip", "newlayout.zip", 100, "alice", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = "completed"
	taskID := task.ID
	taskDir := filepath.Join(mgr.CloudDirFor("alice"), task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "newlayout.zip"), []byte("new layout data"), 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}

	// 为 alice mux 追加云归档 handler（注入 alice actor ctx）
	aliceMux := env.mux["alice"]
	aliceMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.archiveTaskHandler()(w, r)
	})
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/cloud/tasks/"+taskID+"/archive", strings.NewReader(body))
		req.Header.Set(headerContentType, contentTypeJSON)
		rr := httptest.NewRecorder()
		aliceMux.ServeHTTP(rr, req)
		return rr
	}

	// 同名预置在 alice/archive/ → 409（冲突检查落 archive 桶）
	pre := filepath.Join(root, "alice", "archive", "preexist.tar.gz")
	if err := os.MkdirAll(filepath.Dir(pre), 0o755); err != nil {
		t.Fatalf("mkdir archive dir: %v", err)
	}
	if err := os.WriteFile(pre, []byte("old archive"), 0o644); err != nil {
		t.Fatalf("write preexisting archive: %v", err)
	}
	if rr := post(`{"archive_name":"preexist.tar.gz"}`); rr.Code != http.StatusConflict {
		t.Fatalf("同名归档预置在 archive 桶应 409, got %d body=%s", rr.Code, rr.Body.String())
	}

	// 创建归档 → 落 <root>/alice/archive/xxx.tar.gz
	if rr := post(`{"archive_name":"xxx.tar.gz"}`); rr.Code != http.StatusOK {
		t.Fatalf("创建归档应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	archivePath := filepath.Join(root, "alice", "archive", "xxx.tar.gz")
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("归档应落 <root>/alice/archive/xxx.tar.gz: %v", err)
	}

	// kind=cloud_archive 下载 filename=xxx.tar.gz → 200
	if code := env.doGet(t, "alice", "/download?filename=xxx.tar.gz&kind=cloud_archive"); code != http.StatusOK {
		t.Fatalf("kind=cloud_archive 下载应 200, got %d", code)
	}
}

// setupCloudArchiveVolumeHandlers 构造带 fake 外部卷 + completed 任务的 handler，
// 供单任务/批量归档 target_volume 的统一转存测试直接调用 handler（不经 HTTP mux）。
func setupCloudArchiveVolumeHandlers(t *testing.T) (*Handlers, *serverMemFS, *cloud.CloudDownloadManager) {
	t.Helper()
	dir, err := os.MkdirTemp("", "archive-volume-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := newAssemblyTestHandlers(t, dir)
	fs := &serverMemFS{files: map[string][]byte{}}
	h.volSet = newFakeVolSetForTransfer("vault", fs)
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		UploadsDir: dir, Storage: cloudStorageManager{m: sm}, TenantFor: h.tenantFor,
		ChecksumStoreFor: h.checksumStoreFor, ListTenants: h.listTenantIDs,
		Logger: testLogger(), Config: defaultCloudDownloadConfig(),
	})
	h.cloudMgr = mgr
	return h, fs, mgr
}

// TestCloudArchive_SingleTask_ToVolume 单任务归档 target_volume：打包转存外部卷
// （owner 前缀键 + tar.gz 内容），不再静默按桶归档。
func TestCloudArchive_SingleTask_ToVolume(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	id := createCompletedTask(t, mgr, "single.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	key := "anonymous/user/archive/s.tar.gz"
	if _, ok := fs.files[key]; !ok {
		t.Fatalf("目标键 %q 缺失（实际 %v）", key, slices.Sorted(maps.Keys(fs.files)))
	}
	if _, err := gzip.NewReader(bytes.NewReader(fs.files[key])); err != nil {
		t.Fatalf("目标应为 tar.gz（gzip 头）: %v", err)
	}
}

// TestCloudArchive_Batch_ToVolume 批量归档 target_volume：多文件打包转存外部卷。
func TestCloudArchive_Batch_ToVolume(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	id1 := createCompletedTask(t, mgr, "a.bin")
	id2 := createCompletedTask(t, mgr, "b.bin")
	body := fmt.Sprintf(`{"task_ids":[%q,%q],"archive_name":"batch.tar.gz","target_volume":"vault"}`, id1, id2)
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/archive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.archiveBatchHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	key := "anonymous/user/archive/batch.tar.gz"
	if _, ok := fs.files[key]; !ok {
		t.Fatalf("目标键 %q 缺失（实际 %v）", key, slices.Sorted(maps.Keys(fs.files)))
	}
}

// TestCloudArchive_SingleTask_ToVolume_RejectsOverwrite 同名归档二次转存 → 409：锁定
// 目标卷装饰链的 WriteIfAbsent + transfer 存在性检查（防静默覆盖）。
func TestCloudArchive_SingleTask_ToVolume_RejectsOverwrite(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	id := createCompletedTask(t, mgr, "single.bin")
	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
			strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"vault"}`))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.archiveTaskHandler()(rec, req)
		return rec.Code
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("首次转存应 200，got %d", code)
	}
	if code := post(); code != http.StatusConflict {
		t.Fatalf("同名二次转存应 409（拒绝静默覆盖），got %d", code)
	}
}

// TestCloudArchive_SingleTask_ToVolume_LockKey 转存写锁与文件域同键空间（user/<rel>）：
// 预占 user/archive/lock.tar.gz 锁后转存应 409（证明锁键未用带 owner 前缀的 FSPath）。
func TestCloudArchive_SingleTask_ToVolume_LockKey(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	release, ok := h.acquireFileLock("", "user/archive/lock.tar.gz")
	if !ok {
		t.Fatal("预占锁失败")
	}
	defer release()
	id := createCompletedTask(t, mgr, "single.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"lock.tar.gz","target_volume":"vault"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("锁被占时应 409，got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCloudArchive_SingleTask_ToVolume_SecretsRejected secrets 密钥卷非通用转存目标 →
// 400 fail-closed（与 cloud 下载转存 checkTransferVolumePreflight 同语义）。
func TestCloudArchive_SingleTask_ToVolume_SecretsRejected(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	h.volSet = newFakeVolSetForTypedTransfer("secrets", "secrets", &serverMemFS{files: map[string][]byte{}})
	id := createCompletedTask(t, mgr, "single.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"secrets"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("secrets 卷应 400，got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCloudArchive_SingleTask_ToVolume_TargetPath 显式目标路径（客户端由卷 URI 归一）：
// user 桶内子目录 + 文件名直接生效，不再固定 archive/ 前缀。
func TestCloudArchive_SingleTask_ToVolume_TargetPath(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	id := createCompletedTask(t, mgr, "single.bin")
	body := `{"archive_name":"s.tar.gz","target_volume":"vault","target_path":"backups/2026/s.tar.gz"}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	key := "anonymous/user/backups/2026/s.tar.gz"
	if _, ok := fs.files[key]; !ok {
		t.Fatalf("目标键 %q 缺失（实际 %v）", key, slices.Sorted(maps.Keys(fs.files)))
	}
	var resp CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if want := "vault://vault/" + key; resp.File != want {
		t.Fatalf("File=%q want %q", resp.File, want)
	}
}

// TestCloudArchive_SingleTask_ToVolume_Encrypt 归档转存 + 流式加密（encrypt=true）：
// 产物为 tar.gz.aes 密文（不可直接 gzip 解），密钥来自 archive.key_file，解密后 tar 内含源文件。
func TestCloudArchive_SingleTask_ToVolume_Encrypt(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	key := bytes.Repeat([]byte{0x11}, 32)
	keyPath := filepath.Join(h.cfgPtr.Load().StorageRoot, "archive.key")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfgPtr.Load().Archive.KeyFile = keyPath

	id := createCompletedTask(t, mgr, "single.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"vault","encrypt":true}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := fs.files["anonymous/user/archive/s.tar.gz.aes"]
	if len(raw) == 0 {
		t.Fatal("加密转存未落盘")
	}
	if _, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
		t.Fatal("加密产物不应直接 gzip 可解")
	}
	cr, err := files.NewCipherReader("aes-256-gcm", key, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewCipherReader: %v", err)
	}
	gr, err := gzip.NewReader(cr)
	if err != nil {
		t.Fatalf("解密后应可 gzip 解: %v", err)
	}
	defer gr.Close()
	names, err := tarNames(gr)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "single.bin" {
		t.Fatalf("tar 文件列表 %v", names)
	}
}

// TestCloudArchive_SingleTask_ToVolume_Compression 归档转存 + 压缩选型（zstd）。
func TestCloudArchive_SingleTask_ToVolume_Compression(t *testing.T) {
	t.Parallel()
	h, fs, mgr := setupCloudArchiveVolumeHandlers(t)
	id := createCompletedTask(t, mgr, "single.bin")
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive",
		strings.NewReader(`{"archive_name":"s.tar.gz","target_volume":"vault","compression":"zstd"}`))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h.archiveTaskHandler()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := fs.files["anonymous/user/archive/s.tar.gz"]
	if _, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
		t.Fatal("zstd 产物不应被 gzip 解")
	}
	zr, err := compressx.NewReader(compressx.Zstd, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewReader(zstd): %v", err)
	}
	defer zr.Close()
	if _, err := tarNames(zr); err != nil {
		t.Fatalf("zstd 解包: %v", err)
	}
}

// TestCloudArchive_ExtrasRequireTarget 预处理/目标路径选项缺前置条件 → 400（防静默忽略）：
// encrypt/compression 无 target_volume；cipher 无 encrypt；target_path 无 target_volume。
func TestCloudArchive_ExtrasRequireTarget(t *testing.T) {
	t.Parallel()
	h, _, mgr := setupCloudArchiveVolumeHandlers(t)
	id := createCompletedTask(t, mgr, "single.bin")
	for _, body := range []string{
		`{"archive_name":"s.tar.gz","encrypt":true}`,
		`{"archive_name":"s.tar.gz","compression":"zstd"}`,
		`{"archive_name":"s.tar.gz","target_volume":"vault","cipher":"aes-256-gcm"}`,
		`{"archive_name":"s.tar.gz","target_path":"backups/s.tar.gz"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/"+id+"/archive", strings.NewReader(body))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.archiveTaskHandler()(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("选项缺前置条件应 400，body=%s got %d", body, rec.Code)
		}
	}
}

// tarNames 读取 tar 流的条目名列表。
func tarNames(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
}
