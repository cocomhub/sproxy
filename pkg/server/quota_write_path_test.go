// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_write_path_test.go 验证 P4 写路径配额接入 Scope 后的对账：
// 上传 TryReserve→Commit、覆盖 Adjust、删除 ReleaseUsage、超租户上限 507；
// 分块上传 init 预留→complete Commit；云端下载 create 预留→complete Commit→delete 释放。

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// actorUploadDeleteMux 构造把固定 actor 注入请求 ctx 后转发 upload/delete handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 download_owner_test.go 的模式；
// 命名避开 upload_owner_test.go 已有仅含 upload 的 actorUploadMux）。
func actorUploadDeleteMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	return mux
}

// uploadAs 以指定 actor mux 上传 filename 与 body（自动带 X-File-Checksum），返回状态码与响应体。
func uploadAs(t *testing.T, mux *http.ServeMux, filename string, body []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerFileChecksum, sha256hex(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// deleteAs 以指定 actor mux 删除 filename（带 X-File-Checksum），返回状态码。
func deleteAs(t *testing.T, mux *http.ServeMux, filename string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/delete?filename="+filename, nil)
	req.Header.Set(headerFileChecksum, sha256hex(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code
}

// setOwnerQuota 设置 owner 的配额上限（修改共享 Config 的 OwnerQuotas；在首次 quotaFor 前调用）。
func (e *ownerDownloadEnv) setOwnerQuota(owner string, bytes int64) {
	cfg := e.h.cfgPtr.Load()
	if cfg.OwnerQuotas == nil {
		cfg.OwnerQuotas = make(map[string]int64)
	}
	cfg.OwnerQuotas[owner] = bytes
}

// TestQuota_UploadCommitAndDelete 验证上传配额对账：
// 上传 60 → Usage 60；覆盖为 40（versioning 开启走覆盖路径）→ Adjust diff -20 → Usage 40；
// 删除 → ReleaseUsage → Usage 0；bob 配额独立。
func TestQuota_UploadCommitAndDelete(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 100)
	umux := actorUploadDeleteMux(env.h, "alice")

	// 上传 60 字节 → tenant scope Usage()==60
	body60 := []byte(strings.Repeat("a", 60))
	if code, resp := uploadAs(t, umux, "f.txt", body60); code != http.StatusOK {
		t.Fatalf("上传 60 字节应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 60 {
		t.Fatalf("上传 60 后 Usage()=%d want 60", got)
	}

	// 覆盖为 40 → Adjust diff -20 → Usage()==40（versioning 开启）
	env.h.cfgPtr.Load().Versioning.Enabled = true
	body40 := []byte(strings.Repeat("b", 40))
	if code, resp := uploadAs(t, umux, "f.txt", body40); code != http.StatusOK {
		t.Fatalf("覆盖为 40 应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 40 {
		t.Fatalf("覆盖 40 后 Usage()=%d want 40", got)
	}

	// 删除 → ReleaseUsage → Usage()==0
	if code := deleteAs(t, umux, "f.txt", body40); code != http.StatusOK {
		t.Fatalf("删除应 200, got %d", code)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("删除后 Usage()=%d want 0", got)
	}

	// 另一租户 bob 配额独立，alice 打满不影响 bob
	if got := env.h.quotaFor("bob").Usage(); got != 0 {
		t.Fatalf("bob Usage()=%d want 0（独立）", got)
	}
}

// TestQuota_TenantLimitRejected 验证超过租户上限时上传被拒绝（507 InsufficientStorage）。
func TestQuota_TenantLimitRejected(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 10)
	umux := actorUploadDeleteMux(env.h, "alice")

	body := []byte(strings.Repeat("a", 20))
	code, resp := uploadAs(t, umux, "big.txt", body)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("超租户上限应 507, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("507 后 Usage()=%d want 0（不泄漏预留）", got)
	}
}

// TestQuota_CloudDownloadCommitAndDelete 验证云端下载配额对账：
// create 预留 → complete Commit(result.Size)；delete ReleaseUsage → Usage 0。
func TestQuota_CloudDownloadCommitAndDelete(t *testing.T) {
	content := []byte(strings.Repeat("x", 60))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 1000}

	task, err := mgr.SubmitAndStart("url", srv.URL, "cloud.bin", int64(len(content)), t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("云下载应 completed, got %q", task.Status)
	}
	if got := h.quotaFor("alice").Usage(); got != int64(len(content)) {
		t.Fatalf("云下载完成 Usage()=%d want %d", got, len(content))
	}

	if err := mgr.DeleteTask(task.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("删除云任务后 Usage()=%d want 0", got)
	}
}

// TestQuota_CloudTenantLimitRejected 验证云端下载超租户上限时创建被拒（错误可映射 507）。
func TestQuota_CloudTenantLimitRejected(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 10}

	_, err := mgr.SubmitAndStart("url", "https://example.com/big.bin", "big.bin", 20, t.Context(), "alice")
	if err == nil {
		t.Fatal("超租户上限应返回错误")
	}
	if !isStorageFull(err) {
		t.Fatalf("应返回 storage full 错误（可映射 507）, got %v", err)
	}
	// 回滚：全局账本不应残留预留（CreateTask 失败时已回滚）。
	if got := sm.Usage(); got != 0 {
		t.Fatalf("CreateTask 失败后全局账本 Usage()=%d want 0", got)
	}
}

// TestQuota_ArchiveCommitAndConflictRelease 验证云归档配额对账：
// TryReserve(pre) → createTarGz 后 Commit(actual) → Usage == 归档实际大小；
// 同名已存在（errArchiveExists）→ Release() → 不泄漏预留。
func TestQuota_ArchiveCommitAndConflictRelease(t *testing.T) {
	env := newOwnerEnv(t)
	// 需大于归档预占（源大小 + 100MB 占位），否则 TryReserve(pre) 直接 507。
	env.setOwnerQuota("alice", 1<<30)
	root := env.root

	// 装配 cloudMgr + storageMgr（cloudArchiveTask 依赖任务快照与配额对账）
	sm := NewStorageManager(root, 10*1024*1024*1024, nil, testLogger())
	env.h.storageMgr = sm
	mgr := NewCloudDownloadManager(root, sm, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}, func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	})
	env.h.cloudMgr = mgr

	// 创建已完成云任务 + 落盘文件（新布局 <root>/alice/cloud/<id>/<file>）
	task, err := mgr.CreateTask("url", "https://example.com/q.zip", "q.zip", 100, "alice")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = "completed"
	taskDir := filepath.Join(mgr.cloudDirFor("alice"), task.ID)
	if mkErr := os.MkdirAll(taskDir, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(taskDir, "q.zip"), []byte("archive quota data"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	// alice mux 追加云归档 handler（注入 alice actor ctx）
	aliceMux := env.mux["alice"]
	aliceMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.cloudArchiveTask(w, r)
	})
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/cloud/tasks/"+task.ID+"/archive", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		aliceMux.ServeHTTP(rr, req)
		return rr
	}

	// 首次归档 → 200，Scope Usage == 归档文件实际大小
	if rr := post(`{"archive_name":"q1.tar.gz"}`); rr.Code != http.StatusOK {
		t.Fatalf("创建归档应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	archiveAbs := filepath.Join(root, "alice", "archive", "q1.tar.gz")
	info, err := os.Stat(archiveAbs)
	if err != nil {
		t.Fatalf("归档未落盘: %v", err)
	}
	if got := env.h.quotaFor("alice").Usage(); got != info.Size() {
		t.Fatalf("归档后 Usage()=%d want %d（Commit 收敛到实际大小）", got, info.Size())
	}

	// 同名再次归档 → 409，Scope Usage 不变（Release 不泄漏预留）
	before := env.h.quotaFor("alice").Usage()
	if rr := post(`{"archive_name":"q1.tar.gz"}`); rr.Code != http.StatusConflict {
		t.Fatalf("同名归档应 409, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := env.h.quotaFor("alice").Usage(); got != before {
		t.Fatalf("409 后 Usage()=%d want %d（预留已释放）", got, before)
	}
}

// TestQuota_CloudResumeGrowthRejected 验证续传任务增长超过租户上限时被拒绝：
// failTask 已把预留 Commit 掉（reservation==nil、QuotaCommitted=90），resume 下载增长到
// 120（sizeDelta=30）必须触发 scope.TryReserve(30) 容量预检失败 → 任务 failed、Scope 归零
// （Adjust 不做容量检查，预检缺失会让续传增长突破租户上限——审查修复的回归测试）。
func TestQuota_CloudResumeGrowthRejected(t *testing.T) {
	content := make([]byte, 120)
	for i := range content {
		content[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 100}

	// 创建任务（预留 90）+ 模拟失败落 90 字节 partial → failTask Commit(90)（reservation 消费）
	task, err := mgr.CreateTask("url", srv.URL, "resume.bin", 90, "alice")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(mgr.cloudDirFor("alice"), task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, 90)
	for i := range partial {
		partial[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "resume.bin.partial"), partial, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.failTask(task, "simulated failure")
	if got := h.quotaFor("alice").Usage(); got != 90 {
		t.Fatalf("failTask 后 Usage()=%d want 90", got)
	}

	// resume（force 全量重下）→ 增长到 120 超过租户上限 100 → 应失败且 Scope 归零
	if err := mgr.ResumeTask(task.ID, true, "alice"); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, ok := mgr.SnapshotTask(task.ID, "alice")
		if !ok {
			t.Fatal("task disappeared")
		}
		if snap.Status == "failed" || snap.Status == "completed" || snap.Status == "cancelled" {
			if snap.Status != "failed" {
				t.Fatalf("resume 增长超限应 failed, got %q", snap.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume 下载超时未进入终态")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("resume 失败清理后 Usage()=%d want 0（旧 committed 已释放）", got)
	}
}

// TestQuota_ChunkedUploadCommitAndDelete 验证分块上传配额对账：
// init TryReserve(TotalSize) → complete Commit(TotalSize) → Usage == TotalSize。
func TestQuota_ChunkedUploadCommitAndDelete(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	env.h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 100}

	content := []byte(strings.Repeat("a", 60))
	fileChecksum := sha256Hex(content)
	uploadID := "quota-chunk-1"

	code, resp := env.initAs(t, "alice", uploadID, "f.bin", int64(len(content)), 64, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	if c := env.chunkAs(t, "alice", uploadID, 0, content); c != http.StatusOK {
		t.Fatalf("chunk 应 200, got %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != int64(len(content)) {
		t.Fatalf("分块上传完成 Usage()=%d want %d", got, len(content))
	}
}
