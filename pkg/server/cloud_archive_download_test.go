// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// writeCloudArchive 在 <storageRoot>/<tenant>/archive/ 下写入名为 name 的归档文件。
// owner 为空写入 anonymous 租户（未认证）；非空写入该 owner 租户（与服务端按租户隔离一致）。
func writeCloudArchive(t *testing.T, cfgPtr *atomic.Pointer[Config], owner, name string, content []byte) {
	t.Helper()
	tenant := owner
	if tenant == "" {
		tenant = anonymousOwner
	}
	archiveDir := filepath.Join(cfgPtr.Load().StorageRoot(), tenant, "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archiveDir, name), content, 0644); err != nil {
		t.Fatalf("write archive file: %v", err)
	}
}

// TestDownloadCloudArchive_Success 验证 /download?filename=<归档名>&kind=cloud_archive
// 返回 200、内容正确、Content-Disposition 使用归档名（而非内部桶路径）。
func TestDownloadCloudArchive_Success(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	content := []byte("gzip-tar-archive-bytes")
	writeCloudArchive(t, cfgPtr, "", "test-archive.tar.gz", content)

	resp, err := http.Get(url + "/download?filename=test-archive.tar.gz&kind=cloud_archive")
	if err != nil {
		t.Fatalf("get archive: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get(headerContentType); ct != contentTypeOctetStream {
		t.Errorf("expected Content-Type %q, got %q", contentTypeOctetStream, ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "test-archive.tar.gz") {
		t.Errorf("Content-Disposition missing archive name: %q", cd)
	} else if strings.Contains(cd, cloudArchiveDirName) {
		t.Errorf("Content-Disposition should use archive name, not internal dir: %q", cd)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("expected Accept-Ranges bytes, got %q", ar)
	}
	if cs := resp.Header.Get(headerFileChecksum); cs != sha256hex(content) {
		t.Errorf("X-File-Checksum = %q, want %q", cs, sha256hex(content))
	}
	gotBody, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(gotBody, content) {
		t.Errorf("body mismatch: got %q, want %q", string(gotBody), string(content))
	}
}

// TestDownloadCloudArchive_Range 验证 /download 的 Range 能力对归档同样生效（复用 ServeContent）。
func TestDownloadCloudArchive_Range(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	content := []byte("0123456789abcdef")
	writeCloudArchive(t, cfgPtr, "", "range.tar.gz", content)

	req, _ := http.NewRequest("GET", url+"/download?filename=range.tar.gz&kind=cloud_archive", nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 2-5/16") {
		t.Errorf("unexpected Content-Range: %q", cr)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	if string(gotBody) != "2345" {
		t.Errorf("range body = %q, want %q", string(gotBody), "2345")
	}
}

// TestDownloadCloudArchive_Chunk 验证 /download/chunk 的 kind=cloud_archive 分片下载。
func TestDownloadCloudArchive_Chunk(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	content := make([]byte, 10000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	writeCloudArchive(t, cfgPtr, "", "chunked.tar.gz", content)

	// 请求第一片（offset=0, length=4096）
	resp, err := http.Get(url + "/download/chunk?filename=chunked.tar.gz&kind=cloud_archive&offset=0&length=4096")
	if err != nil {
		t.Fatalf("get chunk: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "chunked.tar.gz") {
		t.Errorf("chunk Content-Disposition missing archive name: %q", cd)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content[:4096]) {
		t.Errorf("chunk[0:4096] mismatch")
	}

	// 请求中段（offset=4096, length=4096）
	resp2, err := http.Get(url + "/download/chunk?filename=chunked.tar.gz&kind=cloud_archive&offset=4096&length=4096")
	if err != nil {
		t.Fatalf("get chunk2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	got2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(got2, content[4096:8192]) {
		t.Errorf("chunk[4096:8192] mismatch")
	}

	// 请求尾片（超出剩余长度应截断）
	resp3, err := http.Get(url + "/download/chunk?filename=chunked.tar.gz&kind=cloud_archive&offset=8192&length=4096")
	if err != nil {
		t.Fatalf("get chunk3: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp3.StatusCode)
	}
	got3, _ := io.ReadAll(resp3.Body)
	if !bytes.Equal(got3, content[8192:]) {
		t.Errorf("chunk[8192:] mismatch")
	}
}

// TestDownloadCloudArchive_Stat 验证 HEAD /api/files/stat 支持 kind=cloud_archive。
func TestDownloadCloudArchive_Stat(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	content := []byte("stat-archive-content")
	writeCloudArchive(t, cfgPtr, "", "stat.tar.gz", content)

	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=stat.tar.gz&kind=cloud_archive", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if size := resp.Header.Get("X-File-Size"); size != "20" {
		t.Errorf("X-File-Size = %q, want 20", size)
	}
	if cs := resp.Header.Get(headerFileChecksum); cs != sha256hex(content) {
		t.Errorf("X-File-Checksum = %q, want %q", cs, sha256hex(content))
	}
}

// TestDownloadCloudArchive_InvalidNames 验证 kind=cloud_archive 的归档名防穿越与非法名拒绝。
func TestDownloadCloudArchive_InvalidNames(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	cases := []string{
		"",                   // 空
		".",                  // 点
		"..",                 // 路径穿越
		"../escape.tar.gz",   // 上级目录
		"a/../b.tar.gz",      // 含 ..
		"sub/archive.tar.gz", // 含 /
		"/abs.tar.gz",        // 绝对路径
		"a\\b.tar.gz",        // 反斜杠分隔（Windows 上为路径分隔符）
	}
	for _, name := range cases {
		t.Run("name="+name, func(t *testing.T) {
			resp, err := http.Get(url + "/download?filename=" + name + "&kind=cloud_archive")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 for name %q, got %d", name, resp.StatusCode)
			}
		})
	}
}

// TestDownloadCloudArchive_UnknownKind 验证未知 kind 返回 400（白名单，防任意内部目录访问）。
func TestDownloadCloudArchive_UnknownKind(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	writeCloudArchive(t, cfgPtr, "", "x.tar.gz", []byte("x"))

	for _, kind := range []string{"foo", "internal", "versions", "__cloud__"} {
		t.Run("kind="+kind, func(t *testing.T) {
			resp, err := http.Get(url + "/download?filename=x.tar.gz&kind=" + kind)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 for kind %q, got %d", kind, resp.StatusCode)
			}
		})
	}
}

// TestDownloadCloudArchive_NotFound 验证不存在的归档返回 404。
func TestDownloadCloudArchive_NotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/download?filename=nonexistent.tar.gz&kind=cloud_archive")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestDownloadCloudArchive_EmptyKindRejectsInternal 验证 kind 为空时 .__ 内部目录仍被拒绝
// （ValidateFilePath 保持 .__ 全拒，不因 kind 方案放宽）。
func TestDownloadCloudArchive_EmptyKindRejectsInternal(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	writeCloudArchive(t, cfgPtr, "", "x.tar.gz", []byte("x"))

	// 直接传完整内部路径（不带 kind）必须 400
	resp, err := http.Get(url + "/download?filename=" + cloudArchiveDirName + "/x.tar.gz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for internal dir without kind, got %d", resp.StatusCode)
	}
}

// TestDownloadCloudArchive_NormalDownloadStillWorks 验证 kind 为空时普通文件下载不受影响。
// 普通下载已迁移到 Tenant API：文件落 <storageRoot>/anonymous/user/（未认证）下。
func TestDownloadCloudArchive_NormalDownloadStillWorks(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)
	content := []byte("normal-file")
	normalPath := filepath.Join(cfgPtr.Load().StorageRoot(), anonymousOwner, "user", "normal.txt")
	if err := os.MkdirAll(filepath.Dir(normalPath), 0755); err != nil {
		t.Fatalf("mkdir normal dir: %v", err)
	}
	if err := os.WriteFile(normalPath, content, 0644); err != nil {
		t.Fatalf("write normal file: %v", err)
	}

	resp, err := http.Get(url + "/download?filename=normal.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, content) {
		t.Errorf("normal download body mismatch: got %q, want %q", string(got), string(content))
	}
}

// TestDownloadCloudArchive_RequiresAuth 验证 kind 下载走既有认证（access_keys 配置后需签名）。
func TestDownloadCloudArchive_RequiresAuth(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	})
	// 认证租户的归档落在 <root>/<owner>/archive/ 子目录
	writeCloudArchive(t, cfgPtr, testAccessKey, "auth-archive.tar.gz", []byte("auth-content"))

	// 未签名 → 401
	resp, err := http.Get(url + "/download?filename=auth-archive.tar.gz&kind=cloud_archive")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without signature, got %d", resp.StatusCode)
	}

	// 有效签名 → 200（归档是用户主动打包的产出，认证用户即可下载）
	req, _ := http.NewRequest("GET", url+"/download?filename=auth-archive.tar.gz&kind=cloud_archive", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("signed get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid signature, got %d", resp2.StatusCode)
	}
	got, _ := io.ReadAll(resp2.Body)
	if string(got) != "auth-content" {
		t.Errorf("signed body = %q, want %q", string(got), "auth-content")
	}
}

// TestDownloadCloudArchive_OwnerIsolation 验证归档按 owner 隔离：
// 归档落在 <root>/<owner>/archive/，其他认证租户下载同一归档名返回 404。
func TestDownloadCloudArchive_OwnerIsolation(t *testing.T) {
	t.Parallel()
	otherAK := "sk-test-other-000000"
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.AccessKeys = []AccessKeyConfig{
			{Key: testAccessKey, Secret: testAccessSecret},
			{Key: otherAK, Secret: testAccessSecret},
		}
	})
	// 归档只属于 testAccessKey 租户
	writeCloudArchive(t, cfgPtr, testAccessKey, "owner-archive.tar.gz", []byte("owner-content"))

	// 归属者 testAccessKey 可下载 → 200
	req, _ := http.NewRequest("GET", url+"/download?filename=owner-archive.tar.gz&kind=cloud_archive", nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("owner get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "owner-content" {
		t.Fatalf("owner download: status=%d body=%q, want 200 owner-content", resp.StatusCode, string(body))
	}

	// 其他租户 otherAK 访问同一归档名 → 404（路径落在他人 owner 目录，无法解析）
	req2, _ := http.NewRequest("GET", url+"/download?filename=owner-archive.tar.gz&kind=cloud_archive", nil)
	signRequest(req2, otherAK, testAccessSecret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("other get: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for other owner, got %d", resp2.StatusCode)
	}
}

// TestDownloadCloudTask_Kind 验证 kind=cloud_task 下载云任务文件：
// filename 传 <taskID>/<file>（不含 .__ 内部前缀），服务端校验任务 owner 后拼接
// uploadsDir/.__cloud__/<taskID>/<file>。跨租户下载同一任务文件返回 404。
func TestDownloadCloudTask_Kind(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	otherAK := "sk-test-other-000000"
	cfg := Default()
	cfg.UploadsDir = tmpDir
	cfg.AccessKeys = []AccessKeyConfig{
		{Key: testAccessKey, Secret: testAccessSecret},
		{Key: otherAK, Secret: testAccessSecret},
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "test",
		BuildAt:     "test",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	// 创建属于 testAccessKey 的任务并写入云端文件（按 owner 落 <root>/<tenant>/cloud/<taskID>/）
	task, err := h.cloudMgr.CreateTask("url", "http://example.com/file", "file.txt", 10, testAccessKey)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	taskDir := filepath.Join(h.cloudMgr.cloudDirFor(testAccessKey), task.ID)
	if mkErr := os.MkdirAll(taskDir, 0755); mkErr != nil {
		t.Fatalf("mkdir task dir: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(taskDir, "file.txt"), []byte("cloud-content"), 0644); wErr != nil {
		t.Fatalf("write cloud file: %v", wErr)
	}

	// 归属者下载 → 200 + 内容正确
	dlURL := ts.URL + "/download?filename=" + url.QueryEscape(task.ID+"/file.txt") + "&kind=cloud_task"
	req, _ := http.NewRequest("GET", dlURL, nil)
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("owner download: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "cloud-content" {
		t.Fatalf("owner download: status=%d body=%q, want 200 cloud-content", resp.StatusCode, string(body))
	}

	// 其他租户下载同一任务文件 → 404（任务按 owner 隔离）
	req2, _ := http.NewRequest("GET", dlURL, nil)
	signRequest(req2, otherAK, testAccessSecret)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("other download: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for other owner, got %d", resp2.StatusCode)
	}
}
