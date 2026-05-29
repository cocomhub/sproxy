// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestServer 启动一个临时的 httptest.Server，绑定到独立的 uploads 目录。
// 返回服务地址、cfgPtr 及 cleanup 函数。tunnel 路由不挂载（避免要求 32 字节 key）。
func newTestServer(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config], func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "sproxy-test-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}

	cfg := Default()
	cfg.UploadsDir = tmpDir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(cfg.UploadsDir)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "test",
		buildAt:       "test",
		checksumStore: cs,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /", h.webRedirect)

	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		_ = os.RemoveAll(tmpDir)
	}
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
	if _, err := part.Write(body); err != nil {
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
	tmpPath := filepath.Join(cfgPtr.Load().UploadsDir, "bad.txt.tmp")
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatalf("temp file should have been cleaned up: %s", tmpPath)
	}
	finalPath := filepath.Join(cfgPtr.Load().UploadsDir, "bad.txt")
	if _, err := os.Stat(finalPath); err == nil {
		t.Fatalf("final file should not exist: %s", finalPath)
	}
}

func TestUpload_BodyTooLarge(t *testing.T) {
	url, _, cleanup := newTestServer(t, func(c *Config) {
		c.MaxUploadBytes = 32 // 故意设得很小
	})
	defer cleanup()

	body := bytes.Repeat([]byte("A"), 1024)
	status, _ := uploadFile(t, url, "big.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", status)
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

// ---- 下载相关 ----

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

// ---- 路由相关 ----

func TestRedirectRoot(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 不跟随重定向，自己检查 Location
	c := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
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

	cs := NewChecksumStore(tmpDir)

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
	cs2 := NewChecksumStore(tmpDir)
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

	cs := NewChecksumStore(tmpDir)
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
	tmpDir, _ := os.MkdirTemp("", "smoke-*")
	defer os.RemoveAll(tmpDir)

	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	// 32 字节占位 tunnel key
	key := make([]byte, 32)
	RegisterRoutes(context.Background(), mux, &cfgPtr, "v", "t", key)
}
