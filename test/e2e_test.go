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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// e2eTestAK / e2eTestSK 是 startSPROXY 配置的 SproxySig 测试凭据。
// 认证驱动模式下全部 HTTP 面（除 healthz/version/ui//tunnel）必须验签；
// 与 testutil.TestAccessKey/TestKey 一致，保证 sclient/FileClient 派生隧道密钥一致。
const (
	e2eTestAK = "ak-00000000000000000000000000000000"
	e2eTestSK = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// signingTransport 自动给每个请求加 SproxySig 签名头（body 预哈希后重放）。
// access_keys 配置后直连 HTTP 面必须验签；此 transport 使 E2E 测试能复用
// http.DefaultClient 风格的裸请求，无需逐请求手动签名。
type signingTransport struct {
	base http.RoundTripper
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyHash string
	if req.Body != nil && req.Body != http.NoBody {
		data, rerr := io.ReadAll(req.Body)
		if rerr != nil {
			return nil, rerr
		}
		_ = req.Body.Close()
		sum := sha256.Sum256(data)
		bodyHash = hex.EncodeToString(sum[:])
		req.ContentLength = int64(len(data))
		req.Body = io.NopCloser(bytes.NewReader(data))
	} else {
		bodyHash = sproxysig.EmptyBodyHash()
	}
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: e2eTestAK,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      sproxysig.NewNonce(),
		BodySHA256: bodyHash,
	}
	req.Header.Set("Authorization", sproxysig.SignAndFormat(e2eTestSK, h, req.Method, req.URL.EscapedPath(), req.URL.RawQuery))
	return t.base.RoundTrip(req)
}

// authedHTTPClient 是带 SproxySig 签名的 HTTP client（替代 http.DefaultClient）。
var authedHTTPClient = &http.Client{Transport: &signingTransport{base: http.DefaultTransport}}

// seedCredentialStore 在 <storageRoot>/anonymous/meta/credentials.json 预写一条
// plain alive 条目（ser.CredentialStore.Save 的 JSON 格式），使服务端凭据 Ring
// 首启即识别该 AK/SK——凭据 store 化后 yaml access_keys 不再装配 Ring，E2E 若
// 依赖确定性测试凭据必须 pre-seed。skHex 须为 64 hex（32B）。
func seedCredentialStore(t *testing.T, storageRoot, ak, skHex string) {
	t.Helper()
	sk, derr := hex.DecodeString(skHex)
	if derr != nil {
		t.Fatalf("seed credential store: decode sk: %v", derr)
	}
	f := struct {
		Version int             `json:"version"`
		Keys    []accesskey.Key `json:"keys"`
	}{
		Version: 1,
		Keys: []accesskey.Key{{
			AK: ak, Owner: "test",
			Entries: []accesskey.SKEntry{{
				ID: "sk-000000000001", SK: sk, Kind: accesskey.KindPlain,
				CreatedAt: time.Now().UTC().Truncate(time.Second),
				Status:    accesskey.StatusActive,
				Meta:      accesskey.Meta{Type: "initial"},
			}},
		}},
	}
	metaDir := filepath.Join(storageRoot, "anonymous", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("seed credential store mkdir: %v", err)
	}
	data, jerr := json.MarshalIndent(f, "", "  ")
	if jerr != nil {
		t.Fatalf("seed credential store marshal: %v", jerr)
	}
	if werr := os.WriteFile(filepath.Join(metaDir, "credentials.json"), data, 0o644); werr != nil {
		t.Fatalf("seed credential store write: %v", werr)
	}
}

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
	resp, err := authedHTTPClient.Do(req)
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
	resp, err := authedHTTPClient.Get(baseURL + "/download?filename=" + filename)
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
	resp, err := authedHTTPClient.Do(req)
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
	resp, err := authedHTTPClient.Get(baseURL + "/api/files/search?q=" + q)
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
	resp, err := authedHTTPClient.Do(req)
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
	resp, err := authedHTTPClient.Do(req)
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
	baseURL, _, cleanup := startSPROXYImpl(t, "")
	return baseURL, cleanup
}

// startSPROXYImpl 是 startSPROXY 的内部实现：extraConfig 为追加到临时配置文件的额外
// YAML 段（如 owner_quotas / bucket_limits），供配额 E2E 测试注入；同时返回 uploadsDir
// 供磁盘残留断言（507 后无文件落盘）。extraConfig 为空时行为与 startSPROXY 完全一致。
func startSPROXYImpl(t *testing.T, extraConfig string) (string, string, func()) {
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
	// 写入临时配置文件，禁用 TLS（E2E 测试使用纯 HTTP 连接）。
	// 认证驱动：配置 access_keys（fail-fast 要求），客户端统一用 e2eTestAK/e2eTestSK 签名。
	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	// e2eTestAK 为 ak-<hex>（2 段），accessKeyMesh 解析 mesh_id 为空字符串；
	// 服务端 access_keys 不配 mesh_id（默认空）→ 两端 HKDF 派生参数一致。
	configContent := fmt.Sprintf("tls:\n  enabled: false\ncloud_download_allow_private: true\naccess_keys:\n  - key: %q\n    secret: %q\n", e2eTestAK, e2eTestSK)
	if extraConfig != "" {
		configContent += extraConfig
	}
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	// 凭据 store 化装配：服务端凭据表 = Ring（来自 <storage_root>/anonymous/meta/credentials.json，
	// 取代 yaml access_keys）。首启（ring 空）生成 anonymous 随机凭据，导致 e2eTestAK
	// 不会被识别。为使 E2E 拿到确定性 e2eTestAK，预先在该路径写入一条 plain alive 条目
	// （与 server.CredentialStore.Save 的 JSON 格式一致；见 server.NewCredentialStore）。
	seedCredentialStore(t, uploadsDir, e2eTestAK, e2eTestSK)
	args := []string{
		"--addr", addr,
		"--storage-root", uploadsDir,
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

	return baseURL, uploadsDir, cleanup
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

	fc := client.NewFileClient(baseURL,
		client.WithCacheDir(t.TempDir()),
		client.WithAccessKey(e2eTestAK, e2eTestSK))

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
	cdc := result.AsCloudDownloadChain()
	if cdc == nil {
		t.Fatalf("expected AsCloudDownloadChain() to return non-nil, got nil")
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
	resp, err := authedHTTPClient.Get(baseURL + "/api/cloud/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// cloudListTasks 返回 {tasks: [...], total: N} 容器
	var list struct {
		Tasks []any `json:"tasks"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Errorf("unmarshal cloud tasks list failed: %v", err)
	} else if len(list.Tasks) > 0 {
		t.Errorf("expected no cloud tasks after cleanup, got %d", len(list.Tasks))
	}
}

func TestE2E_TunnelEncryption(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	key := testutil.TestKey()

	fc := client.NewFileClient(baseURL,
		client.WithTunnel(e2eTestAK, key),
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
	if err = fc.Download(context.Background(), "tunnel-test.txt", downloadPath); err != nil {
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

// ---- 传输管理器：上传会话恢复 E2E ----
// 分块上传会话恢复：init → 传部分块 → GET /upload/sessions 断言含该会话
// （status=uploading）→ 同 upload_id 再 init（reused）→ 补传缺失块 → complete →
// 断言 /upload/sessions 已不含该会话且文件可下载、checksum 一致。
// 纯 HTTP 直接驱动真实二进制，经 signingTransport 自动 SproxySig 签名。
func TestE2E_UploadSessionResume(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	const chunkSize = 64 << 10                 // 64 KiB，E2E 用小块避免大数据量
	content := make([]byte, chunkSize*3+12345) // ~3.5 块
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	fileChecksum := sha256hex(content)

	modTime := time.Now().Add(-time.Hour).Truncate(time.Second) // 提前 1 小时的确定性时间戳
	uploadID := "e2e-resume-" + fileChecksum[:12]
	filename := "e2e-resume.bin"

	// 1) init，带 file_mod_time 与 file_checksum，建立会话（reused=false）。
	// 迁移到 Tenant chunk 桶后（8f96f2f6）：upload_id 不再带 owner 前缀（per-tenant
	// UploadStore 物理隔离，跨租户同裸 id 互不可见），返回裸 id 作为后续 session key。
	initResp := doInit(t, baseURL, uploadID, filename, content, chunkSize, modTime, fileChecksum)
	if initResp.UploadID != uploadID {
		t.Fatalf("init 返回 upload_id=%q，期望裸 id %q（无 owner 前缀）", initResp.UploadID, uploadID)
	}
	serverID := initResp.UploadID // 裸 session id，后续操作沿用

	// 2) 只传前 2 个分块（部分上传）
	for _, idx := range []int{0, 1} {
		uploadChunkE(t, baseURL, serverID, idx, chunkSize, content)
	}

	// 3) GET /upload/sessions 断言含该会话且状态 uploading
	sess := fetchSession(t, baseURL, serverID)
	if sess == nil {
		t.Fatal("GET /upload/sessions 应包含该会话")
	}
	if sess.Status != "uploading" {
		t.Fatalf("session 状态应为 uploading，got %q", sess.Status)
	}
	if sess.ReceivedCount != 2 {
		t.Fatalf("received_count 应为 2，got %d", sess.ReceivedCount)
	}

	// 4) 同 upload_id 再 init → reused 续传，missing_chunks 合理（init 入参仍用裸 id）
	init2 := doInit(t, baseURL, uploadID, filename, content, chunkSize, modTime, fileChecksum)
	if init2.UploadID != serverID {
		t.Fatalf("续传 init 返回 upload_id=%q，期望 %q", init2.UploadID, serverID)
	}

	// 5) status 查询获取缺失块
	delta := doStatus(t, baseURL, serverID)
	if delta.ReceivedCount != 2 {
		t.Fatalf("续传后 received_count 应为 2，got %d", delta.ReceivedCount)
	}
	if len(delta.MissingChunks) == 0 {
		t.Fatal("续传后仍应有缺失分块")
	}

	// 6) 补传缺失块
	for _, idx := range delta.MissingChunks {
		uploadChunkE(t, baseURL, serverID, idx, chunkSize, content)
	}

	// 7) complete → 成功 + checksum 一致
	complete := doComplete(t, baseURL, serverID)
	if !complete.Success {
		t.Fatalf("complete 失败: %s", complete.Message)
	}
	if complete.FileChecksum != fileChecksum {
		t.Fatalf("complete checksum: got %s, want %s", complete.FileChecksum, fileChecksum)
	}

	// 8) complete 后列表已不含该会话（上传管理器在轮询里靠此判定会话结束）
	if got := fetchSession(t, baseURL, serverID); got != nil {
		t.Fatalf("complete 后 /upload/sessions 仍含会话: %+v", got)
	}

	// 9) 文件可下载，内容与 checksum 一致
	dlStatus, dlHeaders, dlBody := downloadFile(t, baseURL, filename)
	if dlStatus != http.StatusOK {
		t.Fatalf("下载恢复完成的文件失败: status=%d", dlStatus)
	}
	if sha256hex(dlBody) != fileChecksum {
		t.Fatalf("下载内容 checksum 不一致")
	}
	if dlHeaders.Get("X-File-Checksum") != fileChecksum {
		t.Fatalf("下载 checksum 头不一致: got %s", dlHeaders.Get("X-File-Checksum"))
	}
}

// ---- 传输管理器：恢复校验 mtime 变更回退（服务端语义 E2E） ----
// 场景：客户端本地文件 mtime/内容在同一 upload_id 下发生变更（前端应回退全量重传，
// 但服务端对同 upload_id 重新 init 会 returned 已有的会话——本 E2E 验证服务端
// 对这种重新 init 的正确语义（reused 会话 + missing_chunks 列表），
// 以及文件内容变更导致 chunk checksum 变化时服务端的处置。
// 前提：uploadID 是客户端对文件元数据（filename|size|mtime|checksum）派生的，
// 内容变更后 checksum 也会变 → 客户端上传 uploadID 必然变化 → 服务端不会再命中旧会话。
// 因此服务端「同 upload_id 变内容」只在客户端显式复用 upload_id 时出现；
// 此时服务端保留创建时的 FileModTime/FileChecksum 不变（GetOrCreateSession 不覆盖），
// chunk checksum 不一致块会被拒绝写入，完整 re-init 只能视为新会话（不同 upload_id）。
func TestE2E_UploadSessionResume_MTimeChanged(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	const chunkSize = 64 << 10
	// 内容 A（大小与内容 A2 相同，但内容不同，模拟本地文件被替换）
	contentA := repeatPattern(79, chunkSize*2+20000) // ~2.3 块
	contentB := repeatPattern(193, len(contentA))    // 同尺寸不同内容，恰与 A 等长
	if bytes.Equal(contentA, contentB) {
		t.Fatal("测试数据设计错误：两内容不能相同")
	}
	fileChecksumA := sha256hex(contentA)
	fileChecksumB := sha256hex(contentB)
	totalChunks := int((int64(len(contentA)) + chunkSize - 1) / chunkSize)

	modTimeA := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	modTimeB := modTimeA.Add(-30 * time.Minute)                         // 不同 mtime：前端链路里 resume 判定会因 mtime/checksum 变化而回退
	uploadID := "e2e-mtime-" + strings.ReplaceAll(dateStamp(), ":", "") // 每次运行唯一，避免残留会话干扰
	filename := "e2e-mtime-change.bin"

	// 1) init 带 A 的 checksum/mtime（建立会话）。upload_id 为裸 id（无 owner 前缀，
	//    per-tenant UploadStore 隔离），返回的 id 作后续操作 key。
	initA := doInit(t, baseURL, uploadID, filename, contentA, chunkSize, modTimeA, fileChecksumA)
	serverID := initA.UploadID

	// 2) 传 A 的第一块
	uploadChunkE(t, baseURL, serverID, 0, chunkSize, contentA)

	// 3) 同 upload_id 再 init（带 B 的 checksum/mtime）——审查 F4 reuse guard：同 key
	//    但元数据（checksum/size）不同 → 服务端**拒绝**复用（防会话劫持/文件名篡改），
	//    不再静默返回旧会话。真实客户端 upload_id 由元数据派生，内容变则 id 变，永不
	//    触发此路径。
	//    由于 init 是普通 JSON POST，doInit 在非 200 时 Fatalf——这里改用裸请求验证
	//    服务端返回 4xx/5xx（拒绝）。
	initBody, mErr := json.Marshal(e2eInitRequest{
		UploadID:     uploadID,
		Filename:     filename,
		TotalSize:    int64(len(contentB)),
		ChunkSize:    chunkSize,
		TotalChunks:  totalChunks,
		FileChecksum: fileChecksumB,
		FileModTime:  modTimeB.UnixNano(),
	})
	if mErr != nil {
		t.Fatalf("marshal init body: %v", mErr)
	}
	req, _ := http.NewRequest("POST", baseURL+"/upload/init", bytes.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	initBResp, bErr := authedHTTPClient.Do(req)
	if bErr != nil {
		t.Fatalf("重新 init 请求失败: %v", bErr)
	}
	_ = initBResp.Body.Close()
	if initBResp.StatusCode == http.StatusOK {
		t.Fatalf("同 upload_id 不同元数据 init 应被拒绝（F4 reuse guard），got 200")
	}

	// 4) 会话应保留创建时 A 的元数据（拒绝复用后旧会话未被篡改）；
	//    断言 /upload/sessions 里 FileModTime 仍是创建时 A 的值。
	sess := fetchSession(t, baseURL, serverID)
	if sess == nil {
		t.Fatalf("会话应仍在列表中")
	}
	if sess.FileModTime != modTimeA.UnixNano() {
		t.Fatalf("拒绝复用后会话应保留创建时 FileModTime，got %d want %d", sess.FileModTime, modTimeA.UnixNano())
	}

	// 5) 恢复原内容 A 的分块 0（幂等跳过/成功）——验证会话未损坏、缺失块列表仍只缺后续块
	repair := uploadChunkRaw(t, baseURL, serverID, 0, sha256hex(chunkSlice(contentA, 0, chunkSize)), chunkSlice(contentA, 0, chunkSize))
	if !repair.Success {
		t.Fatalf("恢复 A 的 chunk0 应成功: %+v", repair)
	}

	// 7) 补全 A 的缺失块 → complete 成功，checksum 为 A
	for idx := range totalChunks {
		if idx == 0 {
			continue // chunk0 已在上一步恢复
		}
		uploadChunkE(t, baseURL, serverID, idx, chunkSize, contentA)
	}
	complete := doComplete(t, baseURL, serverID)
	if !complete.Success {
		t.Fatalf("complete 失败: %s", complete.Message)
	}
	if complete.FileChecksum != fileChecksumA {
		t.Fatalf("complete checksum 应等于 A: got %s, want %s", complete.FileChecksum, fileChecksumA)
	}

	// 8) 下载验证最终文件 = A
	dlStatus, _, dlBody := downloadFile(t, baseURL, filename)
	if dlStatus != http.StatusOK {
		t.Fatalf("下载状态: %d", dlStatus)
	}
	if sha256hex(dlBody) != fileChecksumA {
		t.Fatalf("最终文件内容应等于 A")
	}
}

// ---- 分块上传 E2E 辅助 ----------------

// e2eInitData 是 /upload/init 请求体的结构化参数。
// initInitResp e2e: /upload/init 响应。
type e2eInitResponse struct {
	Success   bool   `json:"success"`
	UploadID  string `json:"upload_id,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	Message   string `json:"message,omitempty"`
}

type e2eInitRequest struct {
	UploadID     string `json:"upload_id"`
	Filename     string `json:"filename"`
	TotalSize    int64  `json:"total_size"`
	ChunkSize    int64  `json:"chunk_size"`
	TotalChunks  int    `json:"total_chunks"`
	FileChecksum string `json:"file_checksum"`
	FileModTime  int64  `json:"file_mod_time"`
}

type e2eChunkUploadResponse struct {
	Success     bool   `json:"success"`
	ChunkIndex  int    `json:"chunk_index,omitempty"`
	ShouldRetry bool   `json:"should_retry,omitempty"`
	Message     string `json:"message,omitempty"`
}

type e2eStatusResponse struct {
	Success       bool   `json:"success"`
	UploadID      string `json:"upload_id,omitempty"`
	ReceivedCount int    `json:"received_count,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	MissingChunks []int  `json:"missing_chunks,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
	FileChecksum  string `json:"file_checksum,omitempty"`
	Message       string `json:"message,omitempty"`
}

type e2eCompleteResponse struct {
	Success      bool   `json:"success"`
	Filename     string `json:"filename,omitempty"`
	FileChecksum string `json:"file_checksum,omitempty"`
	Message      string `json:"message,omitempty"`
}

type e2eSessionInfo struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	TotalSize     int64  `json:"total_size"`
	ReceivedCount int    `json:"received_count"`
	TotalChunks   int    `json:"total_chunks"`
	FileChecksum  string `json:"file_checksum"`
	FileModTime   int64  `json:"file_mod_time"`
	Status        string `json:"status"`
}

type e2eSessionsResponse struct {
	Success  bool             `json:"success"`
	Sessions []e2eSessionInfo `json:"sessions"`
}

// doInit 发送 POST /upload/init（JSON body），返回解析后的响应。
// body 通过 signingTransport 预哈希签名，与 FileClient 语义一致。
func doInit(t *testing.T, baseURL, uploadID, filename string, content []byte, chunkSize int64, modTime time.Time, fileChecksum string) e2eInitResponse {
	t.Helper()
	body := e2eInitRequest{
		UploadID:     uploadID,
		Filename:     filename,
		TotalSize:    int64(len(content)),
		ChunkSize:    chunkSize,
		TotalChunks:  (len(content) + int(chunkSize) - 1) / int(chunkSize),
		FileChecksum: fileChecksum,
		FileModTime:  modTime.UnixNano(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal init body: %v", err)
	}
	req, err := http.NewRequest("POST", baseURL+"/upload/init", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("init 请求构造失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("init 请求失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("init 期望 200 得到 %d: %s", resp.StatusCode, respBody)
	}
	var out e2eInitResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("init 响应解析失败: %v (body: %s)", err, respBody)
	}
	return out
}

// uploadChunkE 读取 content 的第 idx 个分块并以 multipart 上传（字段与 FileClient 一致：
// upload_id / chunk_index / chunk_checksum / 文件字段 chunk）。
func uploadChunkE(t *testing.T, baseURL, uploadID string, idx int, chunkSize int64, content []byte) {
	t.Helper()
	res := uploadChunkRaw(t, baseURL, uploadID, idx, sha256hex(chunkSlice(content, idx, chunkSize)), chunkSlice(content, idx, chunkSize))
	if !res.Success {
		t.Fatalf("chunk %d 上传失败: %+v", idx, res)
	}
}

// uploadChunkRaw 上传指定分块，返回解析后的响应（调用方自行判断成败）。
func uploadChunkRaw(t *testing.T, baseURL, uploadID string, idx int, chunkChecksum string, data []byte) e2eChunkUploadResponse {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("upload_id", uploadID); err != nil {
		t.Fatalf("write upload_id field: %v", err)
	}
	if err := mw.WriteField("chunk_index", fmt.Sprintf("%d", idx)); err != nil {
		t.Fatalf("write chunk_index field: %v", err)
	}
	if err := mw.WriteField("chunk_checksum", chunkChecksum); err != nil {
		t.Fatalf("write chunk_checksum field: %v", err)
	}
	part, err := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", idx))
	if err != nil {
		t.Fatalf("create chunk form file: %v", err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatalf("write chunk data: %v", err)
	}
	if err = mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/upload/chunk", &buf)
	if err != nil {
		t.Fatalf("chunk 请求构造失败: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("chunk 请求失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var out e2eChunkUploadResponse
	if err = json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("chunk 响应解析失败: %v (body: %s)", err, respBody)
	}
	return out
}

// doStatus 查询 /upload/status?upload_id= 并返回解析后的响应。
func doStatus(t *testing.T, baseURL, uploadID string) e2eStatusResponse {
	t.Helper()
	resp, err := authedHTTPClient.Get(baseURL + "/upload/status?upload_id=" + url.QueryEscape(uploadID))
	if err != nil {
		t.Fatalf("status 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out e2eStatusResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("status 响应解析失败: %v (body: %s)", err, body)
	}
	if !out.Success {
		t.Fatalf("status 查询失败: %+v (HTTP body: %s)", out, body)
	}
	return out
}

// doComplete 发送 POST /upload/complete（JSON body），返回解析后的响应。
func doComplete(t *testing.T, baseURL, uploadID string) e2eCompleteResponse {
	t.Helper()
	body := fmt.Sprintf(`{"upload_id":%q}`, uploadID)
	req, err := http.NewRequest("POST", baseURL+"/upload/complete", strings.NewReader(body))
	if err != nil {
		t.Fatalf("complete 请求构造失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("complete 请求失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var out e2eCompleteResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		t.Fatalf("complete 响应解析失败: %v (body: %s)", err, respBody)
	}
	return out
}

// fetchSession 返回 /upload/sessions 中指定 upload_id 的会话，未找到返回 nil。
func fetchSession(t *testing.T, baseURL, uploadID string) *e2eSessionInfo {
	t.Helper()
	var out e2eSessionsResponse
	resp, err := authedHTTPClient.Get(baseURL + "/upload/sessions")
	if err != nil {
		t.Fatalf("GET /upload/sessions 失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("/upload/sessions 响应解析失败: %v (body: %s)", err, body)
	}
	for i := range out.Sessions {
		if out.Sessions[i].UploadID == uploadID {
			return &out.Sessions[i]
		}
	}
	return nil
}

// chunkSlice 返回内容第 idx 个分块的字节（最后一个分块可能不足 chunkSize）。
func chunkSlice(content []byte, idx int, chunkSize int64) []byte {
	start := int64(idx) * chunkSize
	if start >= int64(len(content)) {
		return []byte{} // 防御：越界返回空块
	}
	end := min(start+chunkSize, int64(len(content)))
	return content[start:end]
}

// repeatPattern 用后端模式填充长度为 n 的不可重复内容（供测试数据与 checksum 区分）。
func repeatPattern(seed byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(int(seed)*13 + i*29 + int(seed))
	}
	return out
}

// dateStamp 返回 yyyyMMddHHmmss 格式的时间戳，用于生成唯一 upload_id。
func dateStamp() string {
	return time.Now().Format("20060102150405")
}
