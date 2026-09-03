// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_write_path_test.go 验证 P4 写路径配额接入 Scope 后的对账：
// 上传 TryReserve→Commit、覆盖 Adjust、删除 ReleaseUsage、超租户上限 507；
// 分块上传 init 预留→complete Commit；云端下载 create 预留→complete Commit→delete 释放。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// uploadAsPath 以指定 actor mux 上传到 remotePath（子目录经 X-File-Path 头指定——
// multipart 的 FileName() 会 filepath.Base 掉目录，子目录路径必须走 X-File-Path）。
func uploadAsPath(t *testing.T, mux *http.ServeMux, remotePath string, body []byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(remotePath))
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
	req.Header.Set("X-File-Path", remotePath)
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
// 上传 60 → Usage 60；覆盖为 40（versioning 开启走覆盖路径，saveVersion 先保存旧版本
// 60 字节到 version 桶 → Usage 100=user40+version60）→ Adjust diff -20 使 user 桶收敛 40；
// 删除 user 文件 → ReleaseUsage 释放 user 40，version 桶占用保留（Usage 60）；bob 配额独立。
func TestQuota_UploadCommitAndDelete(t *testing.T) {
	env := newOwnerEnv(t)
	// 配额需足够容纳版本字节（旧文件 60）叠加覆盖写预留（新 40）：60+60+40=160 ≤ 200。
	env.setOwnerQuota("alice", 200)
	umux := actorUploadDeleteMux(env.h, "alice")

	// 上传 60 字节 → tenant scope Usage()==60
	body60 := []byte(strings.Repeat("a", 60))
	if code, resp := uploadAs(t, umux, "f.txt", body60); code != http.StatusOK {
		t.Fatalf("上传 60 字节应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 60 {
		t.Fatalf("上传 60 后 Usage()=%d want 60", got)
	}

	// 覆盖为 40（versioning 开启）：saveVersion 把旧版本 60 字节计入 version 桶，
	// user 桶 Adjust(60→40)。Usage = user40 + version60 = 100。
	env.h.cfgPtr.Load().Versioning.Enabled = true
	body40 := []byte(strings.Repeat("b", 40))
	if code, resp := uploadAs(t, umux, "f.txt", body40); code != http.StatusOK {
		t.Fatalf("覆盖为 40 应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 100 {
		t.Fatalf("覆盖 40 后 Usage()=%d want 100（user 40 + version 60）", got)
	}

	// 删除 user 文件 → ReleaseUsage 释放 user 40；version 桶占用保留。
	if code := deleteAs(t, umux, "f.txt", body40); code != http.StatusOK {
		t.Fatalf("删除应 200, got %d", code)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 60 {
		t.Fatalf("删除后 Usage()=%d want 60（version 桶占用保留）", got)
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

// TestCloudArchive_DeleteReleasesScope 验证 P5 归档删除释放（审查重要 2）：
// 归档创建 → archive 桶 Scope 计入实际大小；deleteCloudArchive 删除文件并释放 Scope，
// 不依赖周期扫描自愈。
func TestCloudArchive_DeleteReleasesScope(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 1<<30)
	sm := NewStorageManager(env.root, 10*1024*1024*1024, nil, testLogger())
	env.h.storageMgr = sm
	mgr := NewCloudDownloadManager(env.root, sm, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}, func(owner string) *quota.Scope {
		return env.h.quotaBucketFor(owner, "cloud")
	})
	env.h.cloudMgr = mgr

	// 创建已完成云任务 + 落盘文件
	task, err := mgr.CreateTask("url", "https://example.com/del.zip", "del.zip", 100, "alice")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	task.Status = "completed"
	taskDir := filepath.Join(mgr.cloudDirFor("alice"), task.ID)
	if mkErr := os.MkdirAll(taskDir, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(taskDir, "del.zip"), []byte("archive delete test"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	// alice mux 追加归档 handler
	aliceMux := env.mux["alice"]
	aliceMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.cloudArchiveTask(w, r)
	})
	req := httptest.NewRequest("POST", "/api/cloud/tasks/"+task.ID+"/archive", strings.NewReader(`{"archive_name":"del.tar.gz"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	aliceMux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("创建归档应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	archiveAbs := filepath.Join(env.root, "alice", "archive", "del.tar.gz")
	info, err := os.Stat(archiveAbs)
	if err != nil {
		t.Fatalf("归档未落盘: %v", err)
	}
	archiveScope := env.h.quotaBucketFor("alice", "archive")
	if got := archiveScope.Usage(); got != info.Size() {
		t.Fatalf("归档后 archive 桶 Usage()=%d want %d", got, info.Size())
	}

	// 删除归档 → 文件移除 + archive 桶 Scope 释放（不依赖周期扫描）
	if err := env.h.deleteCloudArchive("alice", "del.tar.gz"); err != nil {
		t.Fatalf("deleteCloudArchive: %v", err)
	}
	if _, err := os.Stat(archiveAbs); !os.IsNotExist(err) {
		t.Fatalf("归档文件应已删除, stat err=%v", err)
	}
	if got := archiveScope.Usage(); got != 0 {
		t.Fatalf("删除归档后 archive 桶 Usage()=%d want 0", got)
	}
}

// TestVersionQuota_CommitAndRelease 验证 P5 版本桶配额（审查重要 3）：
// saveVersion 写版本文件后 version 桶 Scope 计入版本字节；删除版本后释放。
func TestVersionQuota_CommitAndRelease(t *testing.T) {
	env := newOwnerEnv(t)
	cfg := env.h.cfgPtr.Load()
	cfg.Versioning.Enabled = true
	cfg.Versioning.MaxVersions = 10
	env.h.cfgPtr.Store(cfg)

	tnt := env.h.tenantFor("alice")
	if tnt == nil {
		t.Fatal("创建 alice 租户失败")
	}
	root := tnt.Root()
	if err := root.MkdirAll("user", 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version quota body")
	f, err := root.OpenFile("user/f.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("创建 user 文件失败: %v", err)
	}
	if _, wErr := f.Write(body); wErr != nil {
		f.Close()
		t.Fatalf("写 user 文件失败: %v", wErr)
	}
	if cErr := f.Close(); cErr != nil {
		t.Fatalf("关闭 user 文件失败: %v", cErr)
	}

	// 保存版本（覆盖前备份）
	vid, err := env.h.saveVersion("f.txt", tnt, "alice")
	if err != nil {
		t.Fatalf("saveVersion: %v", err)
	}
	if vid == 0 {
		t.Fatal("应保存到版本")
	}
	vScope := env.h.quotaBucketFor("alice", "version")
	if vScope == nil {
		t.Fatal("version 桶 Scope 应为非 nil")
	}
	if got := vScope.Usage(); got != int64(len(body)) {
		t.Fatalf("保存版本后 version 桶 Usage()=%d want %d", got, len(body))
	}

	// 删除版本 → 释放 version 桶 Scope
	verDir := filepath.Join(env.root, "alice", "version", "f.txt")
	entries, err := os.ReadDir(verDir)
	if err != nil {
		t.Fatalf("读取版本目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应恰好 1 个版本, got %d", len(entries))
	}
	delMux := http.NewServeMux()
	delMux.HandleFunc("DELETE /api/versions", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.deleteVersionHandler(w, r)
	})
	delReq := httptest.NewRequest("DELETE", "/api/versions?filename=f.txt&version_id="+entries[0].Name(), nil)
	delRR := httptest.NewRecorder()
	delMux.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("删除版本应 200, got %d body=%s", delRR.Code, delRR.Body.String())
	}
	if got := vScope.Usage(); got != 0 {
		t.Fatalf("删除版本后 version 桶 Usage()=%d want 0", got)
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
	// I1 桶级断言：合并后的最终文件落 user 桶（chunk 桶不记 committed）。
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != int64(len(content)) {
		t.Fatalf("分块上传完成后 user 桶 Usage()=%d want %d（合并文件落 user 桶）", got, len(content))
	}
	if got := env.h.quotaBucketFor("alice", "chunk").Usage(); got != 0 {
		t.Fatalf("分块上传完成后 chunk 桶 Usage()=%d want 0（chunk 会话目录不记账）", got)
	}

	// 删除合并后的 user 文件 → user 桶 Usage()==0（ReleaseUsage 按文件大小释放，防桶级错位泄漏）。
	delMux := http.NewServeMux()
	delMux.HandleFunc("POST /delete", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.delete(w, r)
	})
	delReq := httptest.NewRequest("POST", "/delete?filename=f.bin", nil)
	delReq.Header.Set(headerFileChecksum, fileChecksum)
	delRR := httptest.NewRecorder()
	delMux.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("删除分块上传文件应 200, got %d body=%s", delRR.Code, delRR.Body.String())
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 0 {
		t.Fatalf("删除后 user 桶 Usage()=%d want 0", got)
	}
}

// TestQuota_RmdirReleasesUsage 验证 rmdir 递归删除后释放 user 桶配额（I2 修复）：
// 与单文件/批量 delete 一致，删除目录树后 user 桶 committed 归零，不依赖周期扫描自愈。
func TestQuota_RmdirReleasesUsage(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 1000)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.upload(w, r)
	})
	mux.HandleFunc("POST /rmdir", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		env.h.rmdir(w, r)
	})

	// 子目录内上传 2 个文件（40+20=60 字节；子目录路径经 X-File-Path 指定）
	if code, resp := uploadAsPath(t, mux, "dir/a.txt", []byte(strings.Repeat("a", 40))); code != http.StatusOK {
		t.Fatalf("上传 a.txt 应 200, got %d: %s", code, resp)
	}
	if code, resp := uploadAsPath(t, mux, "dir/sub/b.txt", []byte(strings.Repeat("b", 20))); code != http.StatusOK {
		t.Fatalf("上传 b.txt 应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 60 {
		t.Fatalf("rmdir 前 user 桶 Usage()=%d want 60", got)
	}

	// rmdir dir（force=true）→ 递归删除 → user 桶释放 60
	req := httptest.NewRequest("POST", "/rmdir?dirname=dir&force=true", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("rmdir 应 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 0 {
		t.Fatalf("rmdir 后 user 桶 Usage()=%d want 0（递归删除释放配额）", got)
	}
}

// TestQuota_VersionRestoreCommitsUsage 验证版本恢复把版本文件拷回 user 桶时记账（I3 修复）：
// 恢复前删除目标文件（user 桶归零），恢复后 user 桶 == 版本文件大小。
func TestQuota_VersionRestoreCommitsUsage(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 200)
	env.h.cfgPtr.Load().Versioning.Enabled = true

	mux := http.NewServeMux()
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), "alice"))
			hf(w, r)
		}
	}
	mux.HandleFunc("POST /upload", wrap(env.h.upload))
	mux.HandleFunc("POST /delete", wrap(env.h.delete))
	mux.HandleFunc("GET /api/versions", wrap(env.h.listVersionsHandler))
	mux.HandleFunc("POST /api/versions/restore", wrap(env.h.restoreVersionHandler))

	// 上传 60 → user 60；覆盖为 40 → saveVersion（version 桶 60）+ Adjust → user 40
	body60 := []byte(strings.Repeat("a", 60))
	if code, resp := uploadAs(t, mux, "f.txt", body60); code != http.StatusOK {
		t.Fatalf("上传 60 应 200, got %d: %s", code, resp)
	}
	body40 := []byte(strings.Repeat("b", 40))
	if code, resp := uploadAs(t, mux, "f.txt", body40); code != http.StatusOK {
		t.Fatalf("覆盖为 40 应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 40 {
		t.Fatalf("覆盖后 user 桶 Usage()=%d want 40", got)
	}

	// 删除 f.txt → user 桶归零（版本仍在 version 桶）
	delReq := httptest.NewRequest("POST", "/delete?filename=f.txt", nil)
	delReq.Header.Set(headerFileChecksum, sha256hex(body40))
	delRR := httptest.NewRecorder()
	mux.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d", delRR.Code)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 0 {
		t.Fatalf("删除后 user 桶 Usage()=%d want 0", got)
	}

	// 列出版本取 version_id → 恢复 → user 桶 == 版本文件大小（60）
	listReq := httptest.NewRequest("GET", "/api/versions?filename=f.txt", nil)
	listRR := httptest.NewRecorder()
	mux.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("列版本应 200, got %d", listRR.Code)
	}
	var listResp struct {
		Versions []struct {
			VersionID int64 `json:"version_id"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("解析版本列表失败: %v", err)
	}
	if len(listResp.Versions) == 0 {
		t.Fatal("应至少一个版本")
	}
	restoreReq := httptest.NewRequest("POST",
		"/api/versions/restore?filename=f.txt&version_id="+strconv.FormatInt(listResp.Versions[0].VersionID, 10), nil)
	restoreRR := httptest.NewRecorder()
	mux.ServeHTTP(restoreRR, restoreReq)
	if restoreRR.Code != http.StatusOK {
		t.Fatalf("恢复版本应 200, got %d body=%s", restoreRR.Code, restoreRR.Body.String())
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != int64(len(body60)) {
		t.Fatalf("恢复后 user 桶 Usage()=%d want %d（恢复把版本字节计入 user 桶）", got, len(body60))
	}
}

// TestQuota_SyncScopeAdapter 验证 sync pull 的 per-owner 配额解析器（I3 修复）：
// SyncQuotaStore 返回的 resolver 把 TryReserve/Release 落到 owner 的 user 桶 Scope，
// 使 owner_quotas 对 sync 生效（原全局 syncQuotaAdapter 只受 max_storage_bytes 约束）。
func TestQuota_SyncScopeAdapter(t *testing.T) {
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", 100)
	store := env.h.SyncQuotaStore()
	q := store("alice")

	// TryReserve(60) → user 桶 committed 60（syncmgr 单计数器语义：预留即落地）
	if err := q.TryReserve(60, 0); err != nil {
		t.Fatalf("TryReserve(60) 应成功: %v", err)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 60 {
		t.Fatalf("TryReserve(60) 后 user 桶 Usage()=%d want 60", got)
	}
	// 再 TryReserve(60) → 租户聚合 120 > 100 → 拒绝（owner_quotas 生效）
	if err := q.TryReserve(60, 0); err == nil {
		t.Fatal("超租户上限应拒绝（owner_quotas 未生效？）")
	}
	// Release(60) → user 桶归零
	q.Release(60, 0)
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 0 {
		t.Fatalf("Release(60) 后 user 桶 Usage()=%d want 0", got)
	}
	// bob 独立不受影响
	if got := env.h.quotaBucketFor("bob", "user").Usage(); got != 0 {
		t.Fatalf("bob user 桶 Usage()=%d want 0（独立）", got)
	}
	// q 由 SyncQuotaStore() 返回，静态类型已是 syncmgr.QuotaStore（编译期接口保证），
	// 此处仅在调用侧验证 TryReserve/Release 语义可用。
}

// TestQuota_BucketLimits_PathScope 验证 bucket_limits 子目录子 Scope 装配与上限生效（任务 2）：
// 精确路径（user/videos/hd）命中 bucket_limits → quotaBucketFor 返回对应子 Scope，TryReserve
// 超该子 Scope 上限失败（ErrStorageFull，写路径即 507）；同租户其他未配置路径不建立任意
// 子 Scope（quotaBucketFor 返回 nil），且 user 桶整体不受子目录上限约束（仅受 owner_quotas
// 租户总上限兜底）。
//
// 注：本任务阶段写路径仍按物理桶归集（upload 落 "user" 桶，如下 HTTP 上传断言），把写路径
// 解析到精确 path 子 Scope 由后续任务（sync/cloud 装配、任务 8 记账模板）完成；此处用
// TryReserve 直接验证子 Scope 上限机制（即写路径接入后 507 的实现基础）。
func TestQuota_BucketLimits_PathScope(t *testing.T) {
	env := newOwnerEnv(t)
	cfg := env.h.cfgPtr.Load()
	cfg.BucketLimits = map[string]int64{"user/videos/hd": 100}
	cfg.OwnerQuotas = map[string]int64{"alice": 200}
	env.h.cfgPtr.Store(cfg)

	// 精确路径命中：subScope 非 nil、上限 100
	subScope := env.h.quotaBucketFor("alice", "user/videos/hd")
	if subScope == nil {
		t.Fatal("quotaBucketFor(alice, user/videos/hd)=nil（bucket_limits 精确路径应命中）")
	}
	if got := subScope.MaxBytes(); got != 100 {
		t.Fatalf("bucket_limits 子目录 Scope MaxBytes=%d want 100", got)
	}

	// TryReserve(60) 成功 → 子 Scope Reserved 60（预留尚未 Commit）
	if _, err := subScope.TryReserve(60); err != nil {
		t.Fatalf("子目录 TryReserve(60) 应成功: %v", err)
	}
	if got := subScope.Reserved(); got != 60 {
		t.Fatalf("子目录 Scope Reserved()=%d want 60", got)
	}
	// TryReserve(100)（60+100=160 > 100）→ 被该子 Scope 上限拦住（写路径即 507）
	if _, err := subScope.TryReserve(100); err == nil {
		t.Fatal("子目录 TryReserve(100) 应被上限拦住（超 100）")
	} else if !errors.Is(err, quota.ErrStorageFull) {
		t.Fatalf("应返回 ErrStorageFull（可映射 507）, got %v", err)
	}

	// 同租户其他路径不受桶限制：未配置 bucket_limits 的子目录不建立任意子 Scope
	if sc := env.h.quotaBucketFor("alice", "user/other/sub"); sc != nil {
		t.Fatalf("未配置 bucket_limits 的子目录路径应返回 nil（不建立任意子 Scope）")
	}

	// user 桶本身不受子目录上限约束（仅受租户上限）：TryReserve(100) 成功，随即 Release
	// 归还（避免 100B 预留残留把后续 HTTP 上传的租户可用额度挤掉——预留不释放会 507）。
	userScope := env.h.quotaBucketFor("alice", "user")
	if userScope == nil {
		t.Fatal("user 桶 Scope 应为非 nil")
	}
	if rr, err := userScope.TryReserve(100); err != nil {
		t.Fatalf("user 桶 TryReserve(100) 应成功（不被子目录上限约束）: %v", err)
	} else {
		rr.Release()
	}
	if got := userScope.Reserved(); got != 0 {
		t.Fatalf("Release 后 user 桶 Reserved()=%d want 0", got)
	}

	// HTTP 写路径阶段仍按物理桶归集（不回归）：上传到子目录 40 字节 → user 桶 + 租户 Usage 40
	umux := actorUploadDeleteMux(env.h, "alice")
	if code, resp := uploadAsPath(t, umux, "user/videos/hd/a.txt", []byte(strings.Repeat("a", 40))); code != http.StatusOK {
		t.Fatalf("子目录 40 字节应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaFor("alice").Usage(); got != 40 {
		t.Fatalf("上传后租户 Usage()=%d want 40", got)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 40 {
		t.Fatalf("上传后 user 桶 Usage()=%d want 40", got)
	}

	// 任务 8（C：bucket_limits 子目录写路径不接 Scope 的已核实结论）：
	// 上传到受限子目录 user/videos/hd （40 字节 < 子目录上限 100）→ 200，子目录 Scope 不记账
	// （写路径仍按 user 桶聚合），证明 bucket_limits 子目录子 Scope 上限**不截断** 该子目录写路径。
	// 子 Scope 的精确路径命中（quotaBucketFor 返回它）仅对显式 TryReserve 校验生效（上文），
	// 写侧是否接入该子 Scope 由设计确认（本任务范围不接——避免与功能的复杂交互）。
	subScopeAfter := env.h.quotaBucketFor("alice", "user/videos/hd")
	if got := subScopeAfter.Usage(); got != 0 {
		t.Fatalf("上传到子目录后子目录 Scope Usage()=%d want 0（写路径未接入该子 Scope）", got)
	}

	// 上传到受限子目录且超该子目录上限（80 字节，子目录上限 100 但 user/ 桶已累计 40+80=120 <
	// 租户上限 200）→ 仍 200（写路径按 user 桶聚合，不被子目录子 Scope 上限拦截）。
	if code, resp := uploadAsPath(t, umux, "user/videos/hd/big.txt", []byte(strings.Repeat("b", 80))); code != http.StatusOK {
		t.Fatalf("子目录超子目录上限但满足租户总上限应 200（写路径未接入子目录子 Scope）, got %d: %s", code, resp)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 120 {
		t.Fatalf("两文件后 user 桶 Usage()=%d want 120", got)
	}
}
