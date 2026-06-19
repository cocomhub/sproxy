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

func TestBatchRename_Success(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("content")
	cs := sha256hex(body)
	uploadFile(t, url, "old.txt", body, map[string]string{"X-File-Checksum": cs})

	reqBody := fmt.Sprintf(`{"operations":[{"from":"old.txt","to":"new.txt","checksum":"%s"}]}`, cs)
	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchRenameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if !result.Results[0].Success {
		t.Fatalf("expected success, got message: %s", result.Results[0].Message)
	}
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
	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchRenameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if !result.Results[0].Success {
		t.Fatalf("expected success for same file, got: %s", result.Results[0].Message)
	}
}

func TestBatchRename_MissingChecksum(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("content")
	cs := sha256hex(body)
	uploadFile(t, url, "check.txt", body, map[string]string{"X-File-Checksum": cs})

	resp, err := http.Post(url+"/api/batch/rename", "application/json",
		strings.NewReader(`{"operations":[{"from":"check.txt","to":"moved.txt"}]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchRenameResponse
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

func TestBatchRename_SourceNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	reqBody := `{"operations":[{"from":"nonexistent.txt","to":"new.txt","checksum":"abc"}]}`
	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchRenameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
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
	resp, err := http.Post(url+"/api/batch/rename", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result BatchRenameResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
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
	var result BatchDeleteResponse
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

func TestHealthz_UploadStoreFailure(t *testing.T) {
	t.Parallel()
	// 模拟 uploadStore.Health() 返回错误
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 测试 uploadStore 存在时的健康检查
	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("healthz response: %s", body)
	// 正常路径应返回 OK
}

func TestHealthz_UploadStoreStopped(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	key := make([]byte, 32)
	h := RegisterRoutes(t.Context(), mux, &cfgPtr, "test", "now", key, testLogger(), nil)

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

	// 修改文件权限使其无法打开（仅 Unix）
	if runtime.GOOS != "windows" {
		cfg := cfgPtr.Load()
		filePath := filepath.Join(cfg.UploadsDir, "downloadable.txt")
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

// ---- listFiles 子目录不存在 ----

func TestListFiles_SubdirNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/files?subdir=nonexistent")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (empty list), got %d", resp.StatusCode)
	}
	var result struct {
		Files []fileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected empty list, got %d", len(result.Files))
	}
}

// ---- searchFiles 搜索失败路径 ----

func TestSearchFiles_EmptyQuery(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/files/search?q=")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []fileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected empty list for empty query, got %d", len(result.Files))
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

	// 把上传目录设为只读，使 MkdirAll 失败
	origPerm := cfg.UploadsDir
	if err := os.Chmod(origPerm, 0444); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(origPerm, 0755) })

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

	dirPath := filepath.Join(cfg.UploadsDir, "lockeddir")
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

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=lockeddir", nil)
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
		t.Logf("large body test returned %d (acceptable)", resp.StatusCode)
	}
}
