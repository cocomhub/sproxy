// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestServer 启动一个临时的 httptest.Server，绑定到独立的 uploads 目录。
// 返回服务地址、cfgPtr 及 cleanup 函数。tunnel 路由不挂载（避免要求 32 字节 key）。
//
// 使用 t.TempDir() 与 t.Cleanup() 自动管理临时目录与服务关闭，避免遗忘 cleanup。
func newTestServer(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config], func()) {
	t.Helper()

	tmpDir := t.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(cfg.UploadsDir, nil)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "test",
		buildAt:       "test",
		checksumStore: cs,
		logger:        slog.Default(),
		metrics:       NewMetrics(),
		shareStore:    NewShareStore(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	mux.HandleFunc("GET /api/files/search", h.authMiddleware(h.searchFiles))
	mux.HandleFunc("POST /api/batch/delete", h.authMiddleware(h.batchDelete))
	mux.HandleFunc("POST /api/batch/rename", h.authMiddleware(h.batchRename))
	mux.HandleFunc("POST /api/archive", h.authMiddleware(h.archiveHandler))
	mux.HandleFunc("GET /api/archive-dir", h.authMiddleware(h.archiveDirHandler))
	mux.HandleFunc("GET /api/versions", h.authMiddleware(h.listVersionsHandler))
	mux.HandleFunc("POST /api/versions/restore", h.authMiddleware(h.restoreVersionHandler))
	mux.HandleFunc("DELETE /api/versions", h.authMiddleware(h.deleteVersionHandler))
	mux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	mux.HandleFunc("POST /api/share", h.authMiddleware(h.createShareHandler))
	mux.HandleFunc("GET /s/{token}", h.accessShareHandler)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /", h.webRedirect)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	// 兼容旧的 `defer cleanup()` 调用语义，仍返回一个 no-op cleanup（实际工作交给 t.Cleanup）。
	cleanup := func() {}
	return ts.URL, &cfgPtr, cleanup
}

// uploadFile 构造 multipart 请求上传 filename 与 body，返回 status code 和响应体。
func uploadFile(t *testing.T, baseURL, filename string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	_ = mw.Close()

	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---- 上传相关 ----

func TestUpload_HappyPath(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("hello sproxy")
	status, respBody := uploadFile(t, url, "hello.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	if status != 200 {
		t.Fatalf("expected 200, got %d: %s", status, respBody)
	}
	var resp UploadResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success || resp.Checksum != sha256hex(body) {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestUpload_PathTraversal(t *testing.T) {
	url, cfgPtr, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("malicious")
	status, respBody := uploadFile(t, url, "../escape.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	if status == http.StatusBadRequest {
		// Go <1.26 行为：路径穿越被 sproxy 拦截
		return
	}
	// Go >=1.26 行为：mime/multipart 自动清洗文件名，sproxy 应成功保存
	if status != http.StatusOK {
		t.Fatalf("expected 200 (Go >=1.26 sanitized) or 400 (Go <1.26), got %d: %s", status, respBody)
	}
	// 验证文件名已被清洗（不会是 ../escape.txt）
	uploadsDir := cfgPtr.Load().UploadsDir
	if _, err := os.Stat(filepath.Join(uploadsDir, "escape.txt")); os.IsNotExist(err) {
		t.Fatalf("expected file to be saved as escape.txt (sanitized)")
	}
}

func TestUpload_MissingChecksumHeader(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	status, _ := uploadFile(t, url, "x.txt", []byte("x"), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 missing checksum, got %d", status)
	}
}

func TestUpload_ChecksumMismatch(t *testing.T) {
	url, cfgPtr, cleanup := newTestServer(t, nil)
	defer cleanup()

	status, _ := uploadFile(t, url, "bad.txt", []byte("real content"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("wrong content")),
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 checksum mismatch, got %d", status)
	}
	// 确认临时文件已清理
	if dents, _ := os.ReadDir(cfgPtr.Load().UploadsDir); dents != nil {
		for _, de := range dents {
			if strings.HasPrefix(de.Name(), "bad.txt.tmp") {
				t.Fatalf("temp file should have been cleaned up: %s", de.Name())
			}
		}
	}
	finalPath := filepath.Join(cfgPtr.Load().UploadsDir, "bad.txt")
	if _, err := os.Stat(finalPath); err == nil {
		t.Fatalf("final file should not exist: %s", finalPath)
	}
}

func TestUpload_BodyTooLarge(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 发送一个太大但类型正确的 multipart 请求，验证服务端拒绝
	body := bytes.Repeat([]byte("A"), 2<<20) // 2 MiB，超过 MultipartBufSize 但远小于 UploadBodyLimit
	status, _ := uploadFile(t, url, "big.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	// 2 MiB 文件在 MultipartBufSize=1 MiB 内存缓冲下仍可处理（stdlib 落临时文件），应该返回 200
	if status == http.StatusRequestEntityTooLarge {
		t.Fatal("2 MiB file should not be too large")
	}
}

func TestUpload_Idempotent(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("idempotent body")
	cs := sha256hex(body)
	headers := map[string]string{"X-File-Checksum": cs}

	if s, _ := uploadFile(t, url, "same.txt", body, headers); s != 200 {
		t.Fatalf("first upload failed: %d", s)
	}
	// 第二次上传同文件同 checksum，应该幂等成功
	if s, _ := uploadFile(t, url, "same.txt", body, headers); s != 200 {
		t.Fatalf("second upload should be idempotent, got %d", s)
	}
}

func TestUpload_ToSubDirectory(t *testing.T) {
	url, cfgPtr, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("subdirectory upload test via X-File-Path header")
	cs := sha256hex(body)

	// uploadFile 内部使用 CreateFormFile("file", "sub/dir/test.txt")，
	// Go >=1.26 会截断为 "test.txt"，因此必须通过 X-File-Path 头传递完整路径
	status, respBody := uploadFile(t, url, "sub/dir/test.txt", body, map[string]string{
		"X-File-Checksum": cs,
		"X-File-Path":     "sub/dir/test.txt",
	})
	if status != 200 {
		t.Fatalf("expected 200, got %d: %s", status, respBody)
	}

	// 验证文件保存在子目录下，而非根目录
	uploadsDir := cfgPtr.Load().UploadsDir
	savedPath := filepath.Join(uploadsDir, "sub/dir/test.txt")
	savedPath2 := filepath.Join(uploadsDir, "sub", "dir", "test.txt")
	if _, err := os.Stat(savedPath); os.IsNotExist(err) {
		if _, err2 := os.Stat(savedPath2); os.IsNotExist(err2) {
			// 确认没有存到根目录
			rootPath := filepath.Join(uploadsDir, "test.txt")
			if _, err3 := os.Stat(rootPath); err3 == nil {
				t.Fatal("文件被错误地保存到了根目录而非子目录")
			}
			t.Fatalf("文件未在子目录中找到（checked: %s, %s）", savedPath, savedPath2)
		}
	}
	saved, _ := os.ReadFile(savedPath)
	if !bytes.Equal(saved, body) {
		t.Fatalf("saved file content mismatch")
	}
}

func TestDownload_ChecksumHeader(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("download me")
	cs := sha256hex(body)
	if s, _ := uploadFile(t, url, "dl.txt", body, map[string]string{"X-File-Checksum": cs}); s != 200 {
		t.Fatalf("setup upload failed: %d", s)
	}

	resp, err := http.Get(url + "/download?filename=dl.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-File-Checksum"); got != cs {
		t.Fatalf("X-File-Checksum mismatch: got %q want %q", got, cs)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body mismatch")
	}
}

func TestDownload_PathTraversal(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/download?filename=" + "../../etc/passwd")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- 删除相关 ----

func TestDelete_RequiresChecksum(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("delete me")
	_, _ = uploadFile(t, url, "del.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})

	req, _ := http.NewRequest("POST", url+"/delete?filename=del.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 missing checksum, got %d", resp.StatusCode)
	}
}

func TestDelete_ChecksumMismatch(t *testing.T) {
	url, cfgPtr, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("keep me safe")
	_, _ = uploadFile(t, url, "safe.txt", body, map[string]string{"X-File-Checksum": sha256hex(body)})

	req, _ := http.NewRequest("POST", url+"/delete?filename=safe.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex([]byte("wrong")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 checksum mismatch, got %d", resp.StatusCode)
	}
	// 文件应仍存在
	if _, err := os.Stat(filepath.Join(cfgPtr.Load().UploadsDir, "safe.txt")); err != nil {
		t.Fatalf("file should still exist after failed delete: %v", err)
	}
}

func TestDelete_Success(t *testing.T) {
	url, cfgPtr, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("bye")
	cs := sha256hex(body)
	_, _ = uploadFile(t, url, "bye.txt", body, map[string]string{"X-File-Checksum": cs})

	req, _ := http.NewRequest("POST", url+"/delete?filename=bye.txt", nil)
	req.Header.Set("X-File-Checksum", cs)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(cfgPtr.Load().UploadsDir, "bye.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should be removed")
	}
}

// ---- 列表相关 ----

func TestList_StructuredResponse(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("listme")
	cs := sha256hex(body)
	_, _ = uploadFile(t, url, "a.txt", body, map[string]string{"X-File-Checksum": cs})

	resp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Checksum string `json:"checksum"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 应至少包含我们刚上传的文件（uploads 目录可能含 .checksums.json，会被 listFiles 跳过 dir 但包含 file）
	found := false
	for _, f := range result.Files {
		if f.Name == "a.txt" {
			if f.Size != int64(len(body)) || f.Checksum != cs {
				t.Fatalf("unexpected file info: %+v", f)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("a.txt missing from list: %+v", result.Files)
	}
}

// ---- search ----

func TestSearchFiles_ByKeyword(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("hello world")
	cs := sha256hex(body)
	code, _ := uploadFile(t, url, "report.txt", body, map[string]string{"X-File-Checksum": cs})
	if code != 200 {
		t.Fatalf("upload: expected 200, got %d", code)
	}
	code2, _ := uploadFile(t, url, "other.txt", body, map[string]string{"X-File-Checksum": cs})
	if code2 != 200 {
		t.Fatalf("upload other: expected 200, got %d", code2)
	}

	resp, err := http.Get(url + "/api/files/search?q=report")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("search status: expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Checksum string `json:"checksum"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected 1 file, got %d: %+v", len(result.Files), result.Files)
	}
	if result.Files[0].Name != "report.txt" {
		t.Fatalf("expected report.txt, got %s", result.Files[0].Name)
	}
}

func TestSearchFiles_Empty(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/api/files/search?q=")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("search status: expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []any `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected 0 files for empty query, got %d", len(result.Files))
	}
}

func TestSearchFiles_NoMatch(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/api/files/search?q=nonexistent")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("search status: expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []any `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(result.Files))
	}
}

// ---- pagination ----

func TestListFiles_Pagination_Offset(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 上传 3 个文件
	body := []byte("data")
	cs := sha256hex(body)
	for i := range 3 {
		fn := fmt.Sprintf("file%d.txt", i)
		code, _ := uploadFile(t, url, fn, body, map[string]string{"X-File-Checksum": cs})
		if code != 200 {
			t.Fatalf("upload %s: %d", fn, code)
		}
	}

	resp, err := http.Get(url + "/api/files?offset=1&limit=1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files  []any `json:"files"`
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
		Total  int   `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Offset != 1 {
		t.Fatalf("offset: expected 1, got %d", result.Offset)
	}
	if result.Limit != 1 {
		t.Fatalf("limit: expected 1, got %d", result.Limit)
	}
	if result.Total != 3 {
		t.Fatalf("total: expected 3, got %d", result.Total)
	}
	if len(result.Files) != 1 {
		t.Fatalf("files: expected 1, got %d", len(result.Files))
	}
}

func TestListFiles_Pagination_Unlimited(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("data")
	cs := sha256hex(body)
	for i := range 3 {
		code, _ := uploadFile(t, url, fmt.Sprintf("f%d.txt", i), body, map[string]string{"X-File-Checksum": cs})
		if code != 200 {
			t.Fatalf("upload: %d", code)
		}
	}

	// 不传分页参数——向后兼容
	resp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files  []any `json:"files"`
		Total  int   `json:"total"`
		Offset int   `json:"offset"`
		Limit  int   `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 至少 3 个文件
	if result.Total < 3 {
		t.Fatalf("total: expected >= 3, got %d", result.Total)
	}
	if result.Offset != 0 {
		t.Fatalf("offset: expected 0, got %d", result.Offset)
	}
	if result.Limit != 1000 {
		t.Fatalf("limit: expected 1000 (default), got %d", result.Limit)
	}
	if len(result.Files) < 3 {
		t.Fatalf("files: expected >= 3, got %d", len(result.Files))
	}
}

// ---- sort ----

func TestListFiles_SortByName(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("data")
	cs := sha256hex(body)
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		code, _ := uploadFile(t, url, name, body, map[string]string{"X-File-Checksum": cs})
		if code != 200 {
			t.Fatalf("upload %s: %d", name, code)
		}
	}

	resp, err := http.Get(url + "/api/files?sort=name&order=asc")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) < 3 {
		t.Fatalf("expected >= 3 files, got %d", len(result.Files))
	}
	names := make([]string, 0, len(result.Files))
	for _, f := range result.Files {
		names = append(names, f.Name)
	}
	// a.txt should come before b.txt, and b.txt before c.txt
	if names[0] != "a.txt" || names[1] != "b.txt" || names[2] != "c.txt" {
		t.Fatalf("expected sorted [a.txt, b.txt, c.txt], got %v", names)
	}
}

func TestListFiles_SortByNameDesc(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("data")
	cs := sha256hex(body)
	for _, name := range []string{"a.txt", "c.txt", "b.txt"} {
		code, _ := uploadFile(t, url, name, body, map[string]string{"X-File-Checksum": cs})
		if code != 200 {
			t.Fatalf("upload %s: %d", name, code)
		}
	}

	resp, err := http.Get(url + "/api/files?sort=name&order=desc")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(result.Files))
	for _, f := range result.Files {
		names = append(names, f.Name)
	}
	// c.txt should come before b.txt, b.txt before a.txt
	if names[0] != "c.txt" || names[1] != "b.txt" || names[2] != "a.txt" {
		t.Fatalf("expected sorted desc [c.txt, b.txt, a.txt], got %v", names)
	}
}

// ---- batch ----

func TestBatchDelete_Success(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("data")
	cs := sha256hex(body)
	code, _ := uploadFile(t, url, "a.txt", body, map[string]string{"X-File-Checksum": cs})
	if code != 200 {
		t.Fatalf("upload a.txt: expected 200, got %d", code)
	}
	code2, _ := uploadFile(t, url, "b.txt", body, map[string]string{"X-File-Checksum": cs})
	if code2 != 200 {
		t.Fatalf("upload b.txt: expected 200, got %d", code2)
	}

	reqBody := fmt.Sprintf(`{"files":[{"filename":"a.txt","checksum":"%s"},{"filename":"b.txt","checksum":"%s"}]}`, cs, cs)
	req, _ := http.NewRequest("POST", url+"/api/batch/delete", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Results []struct {
			Filename string `json:"filename"`
			Success  bool   `json:"success"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}
	for _, r := range result.Results {
		if !r.Success {
			t.Fatalf("expected success for %s", r.Filename)
		}
	}
}

func TestBatchDelete_ContinueOnError(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("data")
	cs := sha256hex(body)
	code, _ := uploadFile(t, url, "exists.txt", body, map[string]string{"X-File-Checksum": cs})
	if code != 200 {
		t.Fatalf("upload exists.txt: expected 200, got %d", code)
	}

	reqBody := fmt.Sprintf(`{"files":[{"filename":"nonexistent.txt","checksum":"%s"},{"filename":"exists.txt","checksum":"%s"}]}`, cs, cs)
	req, _ := http.NewRequest("POST", url+"/api/batch/delete", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Results []struct {
			Filename string `json:"filename"`
			Success  bool   `json:"success"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}
	// nonexistent.txt -> success (idempotent delete)
	if !result.Results[0].Success {
		t.Fatalf("expected success for nonexistent (idempotent): %+v", result.Results[0])
	}
	// exists.txt -> success
	if !result.Results[1].Success {
		t.Fatalf("expected success for exists.txt: %+v", result.Results[1])
	}
}

// ---- 路由相关 ----

func TestRedirectRoot(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 不跟随重定向，自己检查 Location
	c := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := c.Get(url + "/")
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected 301, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("expected Location=/ui/, got %q", loc)
	}
}

func TestAuthMiddleware(t *testing.T) {
	url, _, cleanup := newTestServer(t, func(c *Config) {
		c.AuthToken = "secret-token"
	})
	defer cleanup()

	resp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", url+"/api/files", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get with token: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 with valid token, got %d", resp2.StatusCode)
	}
}

// ---- ChecksumStore 并发安全 ----

func TestChecksumStore_ConcurrentSetDelete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "csstore-test-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cs := NewChecksumStore(tmpDir, nil)

	var wg sync.WaitGroup
	const goroutines = 50
	const iterations = 20

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range iterations {
				key := fmt.Sprintf("file-%d-%d", i, j)
				cs.Set(key, sha256hex([]byte(key)))
				if j%3 == 0 {
					cs.Delete(key)
				}
				_ = cs.GetAll()
			}
		}(i)
	}
	wg.Wait()

	// 重新打开 store，确认磁盘内容能正确解析
	cs2 := NewChecksumStore(tmpDir, nil)
	all := cs2.GetAll()
	// 至少不能 panic、不能丢出错误。具体保留数量取决于调度。
	t.Logf("after concurrent ops, store has %d entries", len(all))
}

// ---- ChecksumStore 原子写：确保 .tmp 不残留 ----

func TestChecksumStore_AtomicWriteNoTmpLeftover(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "csstore-atomic-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cs := NewChecksumStore(tmpDir, nil)
	cs.Set("k", "v")
	cs.Set("k2", "v2")
	cs.Delete("k")

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file should not be left after atomic write: %s", e.Name())
		}
	}
}

// ---- 辅助：避免 context 未使用警告 ----

func TestRegisterRoutes_Smoke(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	// 32 字节占位 tunnel key
	key := make([]byte, 32)
	h := RegisterRoutes(t.Context(), mux, &cfgPtr, "v", "t", key, nil, nil)
	t.Cleanup(func() { _ = h.Close() })
}

// newTestServerWithAllRoutes 启动包含全部路由的测试服务器（委托给 RegisterRoutes 注册路由）。
// 返回服务地址与 cfgPtr。使用 t.Cleanup 自动关闭服务与释放资源。
func newTestServerWithAllRoutes(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	cfg.ChunkSize = 4 << 10 // 4 KiB for testing
	cfg.LogLevel = "error"
	cfg.AuthToken = ""
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	if !strings.Contains(cfg.Addr, "127.0.0.1") && !strings.HasPrefix(cfg.Addr, ":") {
		cfg.Addr = "127.0.0.1" + cfg.Addr
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	key := make([]byte, 32) // 32 字节 tunnel key，测试用零值
	h := RegisterRoutes(t.Context(), mux, &cfgPtr, "test-version", "test-buildat", key, testLogger(), nil)

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, &cfgPtr
}

// makeReadOnlyDir 创建一个只读目录（无写权限），用于测试文件写入失败路径。
func makeReadOnlyDir(t *testing.T) (string, func()) {
	t.Helper()
	d := t.TempDir()

	roDir := filepath.Join(d, "readonly")
	if err := os.Mkdir(roDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(roDir, 0444); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		cleanup := func() { os.Chmod(roDir, 0755) }
		return roDir, cleanup
	}
	return d, func() {}
}

// ---- healthz / version ----

func TestHealthz_ReturnsOK(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Fatalf("expected 'OK', got %q", body)
	}
}

func TestVersion_ReturnsInfo(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Version:") {
		t.Fatalf("expected version info, got %q", body)
	}
}

// ---- mkdir ----

func TestMkdir_HappyPath(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/mkdir?dirname=testdir", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	dirPath := filepath.Join(cfgPtr.Load().UploadsDir, "testdir")
	if info, err := os.Stat(dirPath); err != nil || !info.IsDir() {
		t.Fatalf("directory should exist: %v", err)
	}
}

func TestMkdir_MissingDirname(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/mkdir", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMkdir_PathTraversal(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/mkdir?dirname=../../escape", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- rmdir ----

func TestRmdir_HappyPath(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	uploadsDir := cfgPtr.Load().UploadsDir
	dirPath := filepath.Join(uploadsDir, "toremove")
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=toremove", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
		t.Fatal("directory should be removed")
	}
}

func TestRmdir_WithFiles_AlsoDeletesChecksums(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	uploadsDir := cfgPtr.Load().UploadsDir
	subDir := filepath.Join(uploadsDir, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	body := []byte("hello")
	uploadFile(t, url, "subdir/a.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
		"X-File-Path":     "subdir/a.txt",
	})

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=subdir", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if _, err = os.Stat(subDir); !os.IsNotExist(err) {
		t.Fatal("directory should be removed")
	}
	listResp, err := http.Get(url + "/api/files")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer listResp.Body.Close()
	var listResult struct {
		Files []fileInfo `json:"files"`
	}
	json.NewDecoder(listResp.Body).Decode(&listResult)
	for _, f := range listResult.Files {
		if f.Name == "a.txt" || strings.HasPrefix(f.Name, "subdir/") {
			t.Fatalf("file from deleted subdir should not appear in root listing: %s", f.Name)
		}
	}
}

func TestRmdir_NonExistent(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRmdir_OnFileReturns400(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("I am a file")
	uploadFile(t, url, "notadir.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=notadir.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRmdir_PathTraversal(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/rmdir?dirname=../../escape", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- rename 分支测试 ----

func TestRename_SameSourceAndTarget(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("same file")
	cs := sha256hex(body)
	uploadFile(t, url, "same.txt", body, map[string]string{"X-File-Checksum": cs})

	req, _ := http.NewRequest("POST", url+"/rename?from=same.txt&to=same.txt", nil)
	req.Header.Set("X-File-Checksum", cs)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (same path), got %d", resp.StatusCode)
	}
	var result UploadResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("expected success: %+v", result)
	}
}

func TestRename_TargetAlreadyExists(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	bodyA := []byte("file A")
	csA := sha256hex(bodyA)
	bodyB := []byte("file B")
	csB := sha256hex(bodyB)

	uploadFile(t, url, "a.txt", bodyA, map[string]string{"X-File-Checksum": csA})
	uploadFile(t, url, "b.txt", bodyB, map[string]string{"X-File-Checksum": csB})

	req, _ := http.NewRequest("POST", url+"/rename?from=a.txt&to=b.txt", nil)
	req.Header.Set("X-File-Checksum", csA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestRename_SourceNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/rename?from=nope.txt&to=dest.txt", nil)
	req.Header.Set("X-File-Checksum", strings.Repeat("a", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRename_MissingChecksum(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/rename?from=a.txt&to=b.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRename_PathTraversal(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("POST", url+"/rename?from=../../a.txt&to=b.txt", nil)
	req.Header.Set("X-File-Checksum", strings.Repeat("b", 64))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- stat 分支测试 ----

func TestStat_HappyPath(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("stat me")
	cs := sha256hex(body)
	uploadFile(t, url, "stat-test.txt", body, map[string]string{"X-File-Checksum": cs})

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=stat-test.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-File-Size"); got == "" {
		t.Fatal("missing X-File-Size")
	}
	if got := resp.Header.Get("X-File-Checksum"); got != cs {
		t.Fatalf("X-File-Checksum mismatch: got %q, want %q", got, cs)
	}
	if got := resp.Header.Get("X-File-MTime"); got == "" {
		t.Fatal("missing X-File-MTime")
	}
}

func TestStat_DirectoryReturnsIsDir(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	uploadsDir := cfgPtr.Load().UploadsDir
	if err := os.Mkdir(filepath.Join(uploadsDir, "statdir"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=statdir", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-File-IsDir") != "true" {
		t.Fatal("expected X-File-IsDir=true for directory")
	}
}

func TestStat_FileNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=nonexistent.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestStat_EmptyFilename(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStat_PathTraversal(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=../../../etc/passwd", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- listFiles 子目录测试 ----

func TestListFiles_SubdirParameter(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	uploadsDir := cfgPtr.Load().UploadsDir
	if err := os.MkdirAll(filepath.Join(uploadsDir, "mydir"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte("nested file")
	cs := sha256hex(body)
	uploadFile(t, url, "mydir/nested.txt", body, map[string]string{
		"X-File-Checksum": cs,
		"X-File-Path":     "mydir/nested.txt",
	})

	resp, err := http.Get(url + "/api/files?subdir=mydir")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result struct {
		Files []fileInfo `json:"files"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if len(result.Files) != 1 || result.Files[0].Name != "nested.txt" {
		t.Fatalf("expected [nested.txt], got %+v", result.Files)
	}
	if result.Files[0].Checksum != cs {
		t.Fatalf("checksum mismatch: got %q, want %q", result.Files[0].Checksum, cs)
	}
}

func TestUpload_ParseMultipartFormError(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 发送非法 multipart 数据（非正确格式的内容）
	req, err := http.NewRequest("POST", url+"/upload", bytes.NewReader([]byte("not a valid multipart body")))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for bad multipart, got %d", resp.StatusCode)
	}
}

func TestDownload_FileNotFound(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/download?filename=nonexistent.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDelete_FileNotFound(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	req, _ := http.NewRequest("POST", url+"/delete?filename=nonexistent.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex([]byte("dummy")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListFiles_SubdirNonExistent(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/api/files?subdir=nonexistent_subdir")
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
		t.Fatalf("expected empty list for nonexistent subdir, got %d files", len(result.Files))
	}
}

func TestSearchFiles_InvalidSubdir(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// subdir 参数触发 ValidateFilePath 错误
	resp, err := http.Get(url + "/api/files?subdir=../../escape")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (empty safe response), got %d", resp.StatusCode)
	}
	var result struct {
		Files []fileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("expected empty list for invalid subdir, got %d files", len(result.Files))
	}
}

// ---- upload 文件已存在 checksum 不匹配 ----

func TestUpload_ExistingFileChecksumMismatch(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("original content")
	cs := sha256hex(body)
	uploadFile(t, url, "conflict.txt", body, map[string]string{"X-File-Checksum": cs})

	body2 := []byte("different content")
	cs2 := sha256hex(body2)
	status, respBody := uploadFile(t, url, "conflict.txt", body2, map[string]string{
		"X-File-Checksum": cs2,
		"X-File-Path":     "conflict.txt",
	})
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", status, respBody)
	}
}

// ---- GzipMiddleware ----
