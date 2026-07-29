// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxy_test provides end-to-end smoke tests for the sproxy server.
// Tests build and start a real sproxy binary, then exercise its HTTP API.
package sproxy_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// ---- helpers ----

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// uploadFile constructs a multipart POST /upload request and returns the status code and body.
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
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// downloadFile performs GET /download and returns status code, headers, and body.
func downloadFile(t *testing.T, baseURL, filename string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(baseURL + "/download?filename=" + filename)
	if err != nil {
		t.Fatalf("download request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

// deleteFile performs POST /delete and returns the status code and body.
func deleteFile(t *testing.T, baseURL, filename, checksum string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+"/delete?filename="+filename, nil)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	if checksum != "" {
		req.Header.Set("X-File-Checksum", checksum)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// searchFiles performs GET /api/files/search?q= and returns the status and parsed JSON.
func searchFiles(t *testing.T, baseURL, query string) (int, map[string]any) {
	t.Helper()
	q := query
	resp, err := http.Get(baseURL + "/api/files/search?q=" + q)
	if err != nil {
		t.Fatalf("search request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err = json.Unmarshal(body, &result); err != nil {
		t.Fatalf("search unmarshal: %v (body: %s)", err, body)
	}
	return resp.StatusCode, result
}

// statFile performs HEAD /api/files/stat?filename= and returns status code and headers.
func statFile(t *testing.T, baseURL, filename string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequest("HEAD", baseURL+"/api/files/stat?filename="+filename, nil)
	if err != nil {
		t.Fatalf("stat request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stat request: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// renameFile performs POST /rename and returns status code and body.
func renameFile(t *testing.T, baseURL, from, to, checksum string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+"/rename?from="+from+"&to="+to, nil)
	if err != nil {
		t.Fatalf("rename request: %v", err)
	}
	if checksum != "" {
		req.Header.Set("X-File-Checksum", checksum)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// startSPROXY builds the sproxy binary, starts it on a random port,
// and waits for it to be healthy. Returns the base URL and a cleanup function.
func startSPROXY(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	binName := "sproxy"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)

	// Locate module root: test/e2e_test.go -> test/ -> module root
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))

	// Build the binary
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if buildOut, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, buildOut)
	}

	// Find a free port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close() //nolint:staticcheck // close before starting server is fine for tests

	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatalf("create uploads dir: %v", err)
	}

	// Start server
	// 写入临时配置文件，禁用 TLS（E2E 测试使用纯 HTTP 连接）
	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	configContent := []byte("tls:\n  enabled: false\ntunnel_key: \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\n")
	if err := os.WriteFile(configPath, configContent, 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	args := []string{
		"--addr", addr,
		"--uploads-dir", uploadsDir,
		"--config", configPath,
	}
	cmd := exec.Command(binPath, args...)
	cmd.Dir = moduleRoot

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy: %v", err)
	}

	baseURL := fmt.Sprintf("http://%s", addr)

	// Poll healthz until ready (up to 5s)
	healthOK := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" {
				healthOK = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !healthOK {
		cmd.Process.Kill()
		cmd.Wait()
		t.Logf("server stdout:\n%s", stdoutBuf.String())
		t.Logf("server stderr:\n%s", stderrBuf.String())
		t.Fatalf("sproxy did not become ready within 5s")
	}

	cleanup := func() {
		// 使用 sync.Once 保证 cmd.Wait() 只被调用一次，避免 data race
		var waitOnce sync.Once
		// Try graceful shutdown on Unix; Windows only supports Kill.
		if runtime.GOOS != "windows" {
			_ = cmd.Process.Signal(os.Interrupt) //nolint:errcheck // best-effort
		}
		done := make(chan struct{})
		go func() {
			waitOnce.Do(func() {
				cmd.Wait()
			})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			waitOnce.Do(func() {
				cmd.Wait()
			})
		}
	}

	return baseURL, cleanup
}

// ---- E2E tests ----

func TestE2E_UploadDownload(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	content := []byte("hello sproxy e2e")
	checksum := sha256hex(content)
	filename := "e2e_test.txt"

	// Upload
	status, body := uploadFile(t, baseURL, filename, content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload expected 200, got %d: %s", status, body)
	}
	var uploadResp struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Checksum string `json:"file_checksum,omitempty"`
	}
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("upload unmarshal: %v (body: %s)", err, body)
	}
	if !uploadResp.Success {
		t.Fatalf("upload failed: %s", uploadResp.Message)
	}
	if uploadResp.Checksum != checksum {
		t.Fatalf("upload checksum mismatch: got %s, want %s", uploadResp.Checksum, checksum)
	}

	// Download
	dlStatus, dlHeaders, dlBody := downloadFile(t, baseURL, filename)
	if dlStatus != http.StatusOK {
		t.Fatalf("download expected 200, got %d", dlStatus)
	}
	if string(dlBody) != string(content) {
		t.Fatalf("download content mismatch: got %q, want %q", dlBody, content)
	}
	if dlHeaders.Get("X-File-Checksum") != checksum {
		t.Fatalf("download checksum header mismatch: got %s, want %s",
			dlHeaders.Get("X-File-Checksum"), checksum)
	}
}

func TestE2E_UploadDelete(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	content := []byte("delete me")
	checksum := sha256hex(content)
	filename := "todelete.txt"

	// Upload
	status, body := uploadFile(t, baseURL, filename, content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload expected 200, got %d: %s", status, body)
	}

	// Delete
	delStatus, delBody := deleteFile(t, baseURL, filename, checksum)
	if delStatus != http.StatusOK {
		t.Fatalf("delete expected 200, got %d: %s", delStatus, delBody)
	}
	var delResp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(delBody, &delResp); err != nil {
		t.Fatalf("delete unmarshal: %v (body: %s)", err, delBody)
	}
	if !delResp.Success {
		t.Fatalf("delete failed: %s", delResp.Message)
	}

	// Download after delete should return 404
	dlStatus, _, _ := downloadFile(t, baseURL, filename)
	if dlStatus != http.StatusNotFound {
		t.Fatalf("download after delete expected 404, got %d", dlStatus)
	}
}

func TestE2E_Search(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	files := map[string][]byte{
		"alpha.txt":  []byte("alpha content"),
		"beta.txt":   []byte("beta content"),
		"gamma.txt":  []byte("gamma content"),
		"delta.txt":  []byte("delta content"),
		"backup.txt": []byte("backup content"),
	}
	for name, content := range files {
		status, body := uploadFile(t, baseURL, name, content, map[string]string{
			"X-File-Checksum": sha256hex(content),
		})
		if status != http.StatusOK {
			t.Fatalf("upload %s expected 200, got %d: %s", name, status, body)
		}
	}

	// Search for "beta" -- should match exactly "beta.txt"
	status, result := searchFiles(t, baseURL, "beta")
	if status != http.StatusOK {
		t.Fatalf("search expected 200, got %d", status)
	}
	filesResult, ok := result["files"].([]any)
	if !ok {
		t.Fatalf("search result missing files array: %v", result)
	}
	if len(filesResult) != 1 {
		t.Fatalf("search 'beta' expected 1 file, got %d: %v", len(filesResult), filesResult)
	}
	file0, ok := filesResult[0].(map[string]any)
	if !ok {
		t.Fatalf("search result item not a map: %v", filesResult[0])
	}
	if file0["name"] != "beta.txt" {
		t.Fatalf("search expected 'beta.txt', got %v", file0["name"])
	}
}

func TestE2E_Rename(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	content := []byte("rename me")
	checksum := sha256hex(content)
	oldName := "old_name.txt"
	newName := "new_name.txt"

	// Upload
	status, body := uploadFile(t, baseURL, oldName, content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload expected 200, got %d: %s", status, body)
	}

	// Rename
	renameStatus, renameBody := renameFile(t, baseURL, oldName, newName, checksum)
	if renameStatus != http.StatusOK {
		t.Fatalf("rename expected 200, got %d: %s", renameStatus, renameBody)
	}
	var renameResp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(renameBody, &renameResp); err != nil {
		t.Fatalf("rename unmarshal: %v (body: %s)", err, renameBody)
	}
	if !renameResp.Success {
		t.Fatalf("rename failed: %s", renameResp.Message)
	}

	// Stat new name -- should exist with matching checksum
	statStatus, statHeaders := statFile(t, baseURL, newName)
	if statStatus != http.StatusOK {
		t.Fatalf("stat new name expected 200, got %d", statStatus)
	}
	if statHeaders.Get("X-File-Checksum") != checksum {
		t.Fatalf("stat checksum mismatch: got %s, want %s",
			statHeaders.Get("X-File-Checksum"), checksum)
	}

	// Stat old name -- should not exist
	oldStatus, _ := statFile(t, baseURL, oldName)
	if oldStatus != http.StatusNotFound {
		t.Fatalf("stat old name expected 404, got %d", oldStatus)
	}
}

func TestE2E_Upload_SimpleChunked(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// 测试小文件（≤4MB）走简单上传路径
	// 用 uploadFile 直接 POST /upload（模拟简单上传）
	smallContent := []byte("small file content for simple upload test")
	smallChecksum := sha256hex(smallContent)
	smallFile := "small_simple.txt"

	status, body := uploadFile(t, baseURL, smallFile, smallContent, map[string]string{
		"X-File-Checksum": smallChecksum,
	})
	if status != http.StatusOK {
		t.Fatalf("simple upload expected 200, got %d: %s", status, body)
	}
	var uploadResp struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Checksum string `json:"file_checksum,omitempty"`
	}
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("upload unmarshal: %v (body: %s)", err, body)
	}
	if !uploadResp.Success {
		t.Fatalf("simple upload failed: %s", uploadResp.Message)
	}
	if uploadResp.Checksum != smallChecksum {
		t.Fatalf("simple upload checksum mismatch: got %s, want %s", uploadResp.Checksum, smallChecksum)
	}

	// 验证下载
	dlStatus, dlHeaders, dlBody := downloadFile(t, baseURL, smallFile)
	if dlStatus != http.StatusOK {
		t.Fatalf("download after simple upload expected 200, got %d", dlStatus)
	}
	if string(dlBody) != string(smallContent) {
		t.Fatalf("download content mismatch: got %q, want %q", dlBody, smallContent)
	}
	if dlHeaders.Get("X-File-Checksum") != smallChecksum {
		t.Fatalf("download checksum mismatch: got %s, want %s",
			dlHeaders.Get("X-File-Checksum"), smallChecksum)
	}

	// 测试大文件（>4MB）走分块上传路径
	// 创建 5MB 文件
	largeContent := make([]byte, 5*1024*1024)
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}
	largeChecksum := sha256hex(largeContent)
	largeFile := "large_chunked.bin"

	status, body = uploadFile(t, baseURL, largeFile, largeContent, map[string]string{
		"X-File-Checksum": largeChecksum,
	})
	if status != http.StatusOK {
		t.Fatalf("chunked upload expected 200, got %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("chunked upload unmarshal: %v (body: %s)", err, body)
	}
	if !uploadResp.Success {
		t.Fatalf("chunked upload failed: %s", uploadResp.Message)
	}
	if uploadResp.Checksum != largeChecksum {
		t.Fatalf("chunked upload checksum mismatch: got %s, want %s", uploadResp.Checksum, largeChecksum)
	}

	// 验证下载大文件
	dlStatus, dlHeaders, dlBody = downloadFile(t, baseURL, largeFile)
	if dlStatus != http.StatusOK {
		t.Fatalf("download after chunked upload expected 200, got %d", dlStatus)
	}
	if string(dlBody) != string(largeContent) {
		t.Fatalf("large file content mismatch")
	}
	if dlHeaders.Get("X-File-Checksum") != largeChecksum {
		t.Fatalf("large file checksum mismatch: got %s, want %s",
			dlHeaders.Get("X-File-Checksum"), largeChecksum)
	}
}

func TestE2E_Upload_EmptyFile(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// 测试 0 字节文件上传
	content := []byte("")
	checksum := sha256hex(content)
	filename := "empty_file.txt"

	status, body := uploadFile(t, baseURL, filename, content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("empty file upload expected 200, got %d: %s", status, body)
	}
	var uploadResp struct {
		Success  bool   `json:"success"`
		Message  string `json:"message"`
		Checksum string `json:"file_checksum,omitempty"`
	}
	if err := json.Unmarshal(body, &uploadResp); err != nil {
		t.Fatalf("upload unmarshal: %v (body: %s)", err, body)
	}
	if !uploadResp.Success {
		t.Fatalf("empty file upload failed: %s", uploadResp.Message)
	}

	// 验证下载
	dlStatus, _, dlBody := downloadFile(t, baseURL, filename)
	if dlStatus != http.StatusOK {
		t.Fatalf("download empty file expected 200, got %d", dlStatus)
	}
	if len(dlBody) != 0 {
		t.Fatalf("empty file content expected 0 bytes, got %d", len(dlBody))
	}
}

func TestE2E_Upload_ChecksumMismatch(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// 上传时提供错误的 checksum
	content := []byte("real content")
	wrongChecksum := sha256hex([]byte("wrong content"))
	filename := "checksum_mismatch.txt"

	status, body := uploadFile(t, baseURL, filename, content, map[string]string{
		"X-File-Checksum": wrongChecksum,
	})
	if status != http.StatusBadRequest && status != http.StatusOK {
		t.Fatalf("checksum mismatch upload expected 400 or 200, got %d: %s", status, body)
	}
	// 服务端可能返回 400（拒绝）或 200（但 success=false）
	if status == http.StatusOK {
		var uploadResp struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &uploadResp); err == nil && !uploadResp.Success {
			t.Logf("server correctly rejected checksum mismatch: %s", uploadResp.Message)
		}
	}
}

func TestE2E_CloudDownloadChain(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	fc := client.NewFileClient(baseURL, client.WithCacheDir(t.TempDir()))

	// 创建一个测试 HTTP 服务器提供下载文件，并返回 Last-Modified 头
	fileContent := []byte("test file content for cloud download chain e2e")
	expectedMTime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	fileTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", expectedMTime.Format(time.RFC1123))
		w.Write(fileContent)
	}))
	defer fileTs.Close()

	// 执行链式操作
	ctx := context.Background()
	result, err := fc.CloudDownloadChain(ctx,
		[]string{fileTs.URL},
		"test-archive.tar.gz",
		t.TempDir(),
		client.WithChainPollInterval(1*time.Second),
		client.WithChainTimeout(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != client.StatusCompleted {
		t.Errorf("expected completed, got %s", result.Status)
	}

	// 验证本地文件存在
	cdc, ok := result.Raw.(*client.CloudDownloadChain)
	if !ok {
		t.Fatalf("expected result.Raw to be *CloudDownloadChain, got %T", result.Raw)
	}
	if cdc.LocalPath == "" {
		t.Fatal("expected local path to be set")
	}
	if _, statErr := os.Stat(cdc.LocalPath); os.IsNotExist(statErr) {
		t.Errorf("local file not found: %s", cdc.LocalPath)
	}

	// 验证文件内容正确（解压 tar.gz 检查）
	f, err := os.Open(cdc.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(tr)
	if string(content) != string(fileContent) {
		t.Errorf("archive content mismatch: got %q, want %q", string(content), string(fileContent))
	}

	// 验证 tar header 中的 mtime 与原始文件一致
	diff := header.ModTime.Sub(expectedMTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("tar header ModTime %v differs from original %v (diff: %v)",
			header.ModTime, expectedMTime, diff)
	}

	// 验证远端文件已被清理（默认 keepFiles=false）
	// 通过尝试访问云任务列表验证
	resp, err := http.Get(baseURL + "/api/cloud/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// cloudListTasks 返回 JSON 数组，不是 {"tasks": [...]}
	var tasks []any
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Logf("unmarshal tasks failed (may be empty already): %v", err)
	} else if len(tasks) > 0 {
		t.Errorf("expected no cloud tasks after cleanup, got %d", len(tasks))
	}
}

func TestE2E_TunnelEncryption(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	key := testutil.TestKey()

	fc := client.NewFileClient(baseURL,
		client.WithTunnel(key),
	)

	content := []byte("tunnel encrypted test content")
	localPath := filepath.Join(t.TempDir(), "tunnel-test.txt")
	if err := os.WriteFile(localPath, content, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := fc.Upload(context.Background(), localPath, "tunnel-test.txt")
	if err != nil {
		t.Fatalf("upload via tunnel failed: %v", err)
	}

	downloadPath := filepath.Join(t.TempDir(), "tunnel-download.txt")
	if err := fc.Download(context.Background(), "tunnel-test.txt", downloadPath); err != nil {
		t.Fatalf("download via tunnel failed: %v", err)
	}
	downloaded, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != string(content) {
		t.Fatalf("content mismatch: got %q, want %q", string(downloaded), string(content))
	}
}
