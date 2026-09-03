// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// ---- batchRename 覆盖率 ----
// 注意：server_extra_test.go 中已有 TestBatchRenameHandler_HappyPath，
// 但该测试发送到错误的路由 "/batch-rename"（应为 "/api/batch/rename"），
// 导致 batchRename handler 实际覆盖率为 0。以下测试修正此问题。

// doBatchRename POST /api/batch/rename 并解码响应。
func doBatchRename(t *testing.T, url, reqBody string) (int, BatchResponse) {
	t.Helper()
	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	var result BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, result
}

// assertBatchRenameOK 断言批量重命名返回 200 且指定索引的结果成功。
func assertBatchRenameOK(t *testing.T, result BatchResponse, index int) {
	t.Helper()
	if len(result.Results) <= index {
		t.Fatalf("expected at least %d results, got %d", index+1, len(result.Results))
	}
	if !result.Results[index].Success {
		t.Fatalf("result[%d] expected success, got message: %s", index, result.Results[index].Message)
	}
}

func TestBatchRename_Success(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("content")
	cs := sha256hex(body)
	uploadFile(t, url, "old.txt", body, map[string]string{"X-File-Checksum": cs})

	reqBody := fmt.Sprintf(`{"operations":[{"from":"old.txt","to":"new.txt","checksum":"%s"}]}`, cs)
	code, result := doBatchRename(t, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	assertBatchRenameOK(t, result, 0)
}

func TestBatchRename_InvalidJSON(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestBatchRename_EmptyOperations(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(`{"operations":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestBatchRename_SameFile(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	reqBody := `{"operations":[{"from":"a.txt","to":"a.txt","checksum":"abc"}]}`
	code, result := doBatchRename(t, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	assertBatchRenameOK(t, result, 0)
}

func TestBatchRename_MissingChecksum(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("content")
	cs := sha256hex(body)
	uploadFile(t, url, "check.txt", body, map[string]string{"X-File-Checksum": cs})

	reqBody := `{"operations":[{"from":"check.txt","to":"moved.txt"}]}`
	code, result := doBatchRename(t, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].Success {
		t.Fatalf("expected failure for missing checksum, got success")
	}
}

func TestBatchRename_SourceNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	reqBody := `{"operations":[{"from":"nonexistent.txt","to":"new.txt","checksum":"abc"}]}`
	code, result := doBatchRename(t, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].Success {
		t.Fatalf("expected failure for missing source, got success")
	}
}

func TestBatchRename_PathTraversal(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	reqBody := `{"operations":[
		{"from":"../a.txt","to":"b.txt","checksum":"abc"},
		{"from":"c.txt","to":"../d.txt","checksum":"abc"}
	]}`
	code, result := doBatchRename(t, url, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}
	// 两个都应为失败（路径穿越被拒绝）
	for i, r := range result.Results {
		if r.Success {
			t.Fatalf("result[%d] should have failed for path traversal", i)
		}
	}
}

// ---- batchDelete 边角情况 ----

func TestBatchDelete_InvalidJSON(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/batch/delete", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestBatchDelete_EmptyFiles(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/batch/delete", "application/json", strings.NewReader(`{"files":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestBatchDelete_MissingChecksum(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("content")
	uploadFile(t, url, "nocheck.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})

	reqBody := `{"files":[{"filename":"nocheck.txt"}]}`
	resp, err := http.Post(url+"/api/batch/delete", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].Success {
		t.Fatalf("expected failure for missing checksum, got success")
	}
}

// ---- healthz uploadStore 故障路径 ----

func TestHealthz_UploadStoreStopped(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})

	// 停止 uploadStore 使其 Health() 返回错误
	_ = h.Close()

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "UploadStore:") {
		t.Fatalf("expected UploadStore error in body, got %q", body)
	}
}

// ---- rename checksum 不匹配 ----

func TestRename_ChecksumMismatch(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("original")
	cs := sha256hex(body)
	uploadFile(t, url, "original.txt", body, map[string]string{"X-File-Checksum": cs})

	// 用错误的 checksum
	req, _ := http.NewRequest("POST", url+"/rename?from=original.txt&to=moved.txt", nil)
	req.Header.Set("X-File-Checksum", strings.Repeat("f", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for checksum mismatch, got %d", resp.StatusCode)
	}
}

// ---- download 打开文件失败 ----

func TestDownload_OpenFileError(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	body := []byte("download test content")
	cs := sha256hex(body)
	uploadFile(t, url, "downloadable.txt", body, map[string]string{"X-File-Checksum": cs})

	// 修改文件权限使其无法打开（仅 Unix）。普通下载已迁移到 Tenant API：
	// 文件落 <storageRoot>/anonymous/user/ 下。
	if runtime.GOOS != "windows" {
		cfg := cfgPtr.Load()
		filePath := filepath.Join(cfg.StorageRoot, anonymousOwner, "user", "downloadable.txt")
		if err := os.Chmod(filePath, 0000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { os.Chmod(filePath, 0644) })

		resp, err := http.Get(url + "/download?filename=downloadable.txt")
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500 for open failure, got %d", resp.StatusCode)
		}
	}
}

// ---- stat checksum 走 checksumStore 与实时计算两条路径 ----

func TestStat_ChecksumFromStore(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("stat with checksum store")
	cs := sha256hex(body)
	uploadFile(t, url, "stat-cs.txt", body, map[string]string{"X-File-Checksum": cs})

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=stat-cs.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-File-Checksum"); got != cs {
		t.Fatalf("expected checksum %s, got %s", cs, got)
	}
	if got := resp.Header.Get("X-File-Size"); got == "" || got == "0" {
		t.Fatalf("expected non-zero X-File-Size, got %s", got)
	}
	if got := resp.Header.Get("X-File-MTime"); got == "" || got == "0" {
		t.Fatalf("expected non-zero X-File-MTime, got %s", got)
	}
}

// ---- dirs.go 中 mkdir os.MkdirAll 失败路径 ----

func TestMkdir_WriteFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission-based mkdir failure test not supported on Windows")
	}
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	cfg := cfgPtr.Load()

	// 把 anonymous 租户根设为只读，使 MkdirAll("user/...") 失败（迁移后 mkdir 在 user 桶内创建）
	tenantRoot := filepath.Join(cfg.StorageRoot, "anonymous")
	if err := os.Chmod(tenantRoot, 0444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(tenantRoot, 0755) })

	req, _ := http.NewRequest("POST", url+"/mkdir?dirname=newdir", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 for write failure, got %d", resp.StatusCode)
	}
}

// ---- rmdir os.RemoveAll 失败路径 ----

func TestRmdir_RemoveAllFailure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permission-based rmdir test not supported on Windows")
	}
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	cfg := cfgPtr.Load()

	dirPath := filepath.Join(cfg.StorageRoot, "anonymous", "user", "lockeddir")
	if err := os.MkdirAll(filepath.Dir(dirPath), 0755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 创建只读子文件，使 RemoveAll 失败
	subFile := filepath.Join(dirPath, "locked.txt")
	if err := os.WriteFile(subFile, []byte("x"), 0444); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 把目录设为只读，使 RemoveAll 中的子文件删除失败
	if err := os.Chmod(dirPath, 0444); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(dirPath, 0755)
		os.RemoveAll(dirPath)
	})

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=lockeddir&force=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 for remove failure, got %d", resp.StatusCode)
	}
}

// ---- upload 解析 multipart 错误 ----

func TestUpload_ParseMultipartBodyLarge(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 发送一个巨大的 body 触发 MaxBytesReader
	largeBody := bytes.Repeat([]byte("A"), 100<<20+1) // >100MB
	resp, err := http.Post(url+"/upload?filename=large.txt", "application/octet-stream",
		bytes.NewReader(largeBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 413 or 400, got %d", resp.StatusCode)
	}
}

// TestRename_CrossSubdir_SymmetricQuota 验证（残余项：rename 跨 bucket_limits 子目录的
// 配额对称转移）：
//   - 上传 40B 到 user/music（无子目录限制，记 user 根）→ rename 到 user/videos/hd（上限 100）
//     → 源键 user 释放 40、目标键 hd 入账 40（子目录配额对 rename 同样封顶）；
//   - 再 rename 60B 同类到 hd（已有 40+60=100 恰满）→ 仍成功（恰好打满）；
//   - 再 rename 1B 到 hd（100+1>100）→ 507 拒绝 + 目标键不泄漏预留（配额不足防绕过）。
func TestRename_CrossSubdir_SymmetricQuota(t *testing.T) {
	env := newOwnerEnv(t)
	cfg := env.h.cfgPtr.Load()
	cfg.BucketLimits = map[string]int64{"user/videos/hd": 100}
	cfg.OwnerQuotas = map[string]int64{"alice": 300}
	env.h.cfgPtr.Store(cfg)
	umux := actorDelRenameMux(env.h, "alice")

	mk := func(n int) []byte { return bytes.Repeat([]byte("a"), n) }
	upload := func(path string, body []byte) {
		t.Helper()
		if code, resp := uploadAsPath(t, umux, path, body); code != http.StatusOK {
			t.Fatalf("上传 %s 应 200, got %d: %s", path, code, resp)
		}
	}
	rename := func(from, to string, body []byte) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/rename?from="+from+"&to="+to, nil)
		req.Header.Set(headerFileChecksum, sha256hex(body))
		rr := httptest.NewRecorder()
		umux.ServeHTTP(rr, req)
		return rr.Code
	}

	upload("music/a.txt", mk(40)) // user 桶 40、hd=0
	// 跨子目录：music → videos/hd。
	if code := rename("music/a.txt", "videos/hd/a.txt", mk(40)); code != http.StatusOK {
		t.Fatalf("rename 到 hd 应 200, got %d", code)
	}
	if got := env.h.quotaScopeFor("alice", "user/videos/hd").Usage(); got != 40 {
		t.Fatalf("rename 后 hd Usage=%d want 40（目标键入账）", got)
	}
	// 源键（user 根）释放：user 桶仍 40（hd 父链聚合），但 music 不再单独占用——验证源键
	// ReleaseUsage 生效：user 桶总量仍 40（=hd 40）。
	// 源文件原在 user/music（无 bucket_limits 子目录配置）→ 归集到 user 桶根。rename 后 user
	// 根总量仍 40（=hd 40 父链聚合）——源键释放生效（music 不再额外占用，非跨键不残留）。
	if got := env.h.quotaScopeFor("alice", "user").Usage(); got != 40 {
		t.Fatalf("rename 后 user 桶根 Usage=%d want 40（=hd 40，源键已释放）", got)
	}
	// 恰好打满：再 rename 60B 到 hd（40+60=100 恰满）→ 成功。
	upload("music/b.txt", mk(60))
	if code := rename("music/b.txt", "videos/hd/b.txt", mk(60)); code != http.StatusOK {
		t.Fatalf("恰好打满 rename 应 200, got %d", code)
	}
	if got := env.h.quotaScopeFor("alice", "user/videos/hd").Usage(); got != 100 {
		t.Fatalf("hd Usage=%d want 100（恰满）", got)
	}
	// 超限：再 rename 1B 到 hd（100+1>100）→ 507 + 目标键不泄漏。
	upload("music/c.txt", mk(1))
	if code := rename("music/c.txt", "videos/hd/c.txt", mk(1)); code != http.StatusInsufficientStorage {
		t.Fatalf("hd 已满 rename 应 507, got %d", code)
	}
	if got := env.h.quotaScopeFor("alice", "user/videos/hd").Reserved(); got != 0 {
		t.Fatalf("507 后 hd Reserved=%d want 0（目标键不泄漏预留）", got)
	}
	// 目标键拒绝时源键始终未动：hd 100、user 根仍 101（music 1 + hd100 父链聚合）。
	if got := env.h.quotaScopeFor("alice", "user").Usage(); got != 101 {
		t.Fatalf("507 后 user 桶根 Usage=%d want 101（music 1 + hd 100）", got)
	}
}

// TestBatchRename_CrossSubdir_SymmetricQuota 验证批量 rename 跨 bucket_limits 子目录的
// 配额对称转移（与单条 rename 对齐）：目标键配额不足单条拒绝（批量继续），不泄漏预留。
func TestBatchRename_CrossSubdir_SymmetricQuota(t *testing.T) {
	env := newOwnerEnv(t)
	cfg := env.h.cfgPtr.Load()
	cfg.BucketLimits = map[string]int64{"user/videos/hd": 100}
	cfg.OwnerQuotas = map[string]int64{"alice": 300}
	env.h.cfgPtr.Store(cfg)
	mux := actorDelRenameMux(env.h, "alice")

	upload := func(path string, n int) {
		t.Helper()
		body := bytes.Repeat([]byte("a"), n)
		if code, resp := uploadAsPath(t, mux, path, body); code != http.StatusOK {
			t.Fatalf("上传 %s 应 200, got %d: %s", path, code, resp)
		}
	}
	upload("music/a.txt", 40)
	upload("music/b.txt", 70)

	// 批量：第 1 条 40B→hd（成功），第 2 条 70B→hd（40+70=110>100 单条拒绝）。
	body := fmt.Sprintf(`{"operations":[{"from":"music/a.txt","to":"videos/hd/a.txt","checksum":"%s"},{"from":"music/b.txt","to":"videos/hd/b.txt","checksum":"%s"}]}`,
		sha256hex(bytes.Repeat([]byte("a"), 40)), sha256hex(bytes.Repeat([]byte("a"), 70)))
	req := httptest.NewRequest("POST", "/api/batch/rename", bytes.NewReader([]byte(body)))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("batch rename 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []BatchOperationResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析 batch 响应: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results 应 2 条, got %d", len(resp.Results))
	}
	if !resp.Results[0].Success {
		t.Fatalf("第 1 条（40B→hd）应成功: %+v", resp.Results[0])
	}
	if resp.Results[1].Success {
		t.Fatalf("第 2 条（70B→hd 超额）应失败: %+v", resp.Results[1])
	}
	// hd 只入账 40（第 1 条）；第 2 条目标键预留失败不泄漏。
	if got := env.h.quotaScopeFor("alice", "user/videos/hd").Usage(); got != 40 {
		t.Fatalf("batch rename 后 hd Usage=%d want 40", got)
	}
	if got := env.h.quotaScopeFor("alice", "user/videos/hd").Reserved(); got != 0 {
		t.Fatalf("batch rename 后 hd Reserved=%d want 0（无泄漏）", got)
	}
	// 第 2 条源仍在 music（user 桶根记 70）。
	if got := env.h.quotaScopeFor("alice", "user").Usage(); got != 110 {
		t.Fatalf("user 桶根 Usage=%d want 110（music 70 + hd 40）", got)
	}
}
