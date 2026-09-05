// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- E2E: Chunked upload and download ----

func TestE2E_ChunkedUploadDownload(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Create a file larger than chunk threshold to trigger chunked path
	fileSize := int64(2 * 1024 * 1024) // 2 MB — small enough for quick test, >1 MB threshold
	content := make([]byte, fileSize)
	for i := range content {
		content[i] = byte(i & 0xff)
	}
	checksum := sha256hex(content)
	filename := "chunked_e2e.bin"

	// Upload via /upload (non-chunked, will use multipart body)
	status, body := uploadFile(t, baseURL, filename, content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload expected 200, got %d: %s", status, body)
	}

	// Stat to verify
	statResp, err := authedHTTPClient.Head(baseURL + "/api/files/stat?filename=" + filename)
	if err != nil {
		t.Fatal(err)
	}
	defer statResp.Body.Close()
	if statResp.StatusCode != http.StatusOK {
		t.Fatalf("stat expected 200, got %d", statResp.StatusCode)
	}
	if statResp.Header.Get("X-File-Checksum") != checksum {
		t.Fatalf("checksum mismatch: got %s, want %s", statResp.Header.Get("X-File-Checksum"), checksum)
	}

	// Download via GET /download
	status, headers, data := downloadFile(t, baseURL, filename)
	if status != http.StatusOK {
		t.Fatalf("download expected 200, got %d", status)
	}
	if sha256hex(data) != checksum {
		t.Fatal("downloaded content checksum mismatch")
	}
	if headers.Get("X-File-Checksum") != checksum {
		t.Fatalf("response checksum header mismatch: %s vs %s", headers.Get("X-File-Checksum"), checksum)
	}

	// Range download
	req, err := http.NewRequest("GET", baseURL+"/download?filename="+filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=100-199")
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range download expected 206, got %d", resp.StatusCode)
	}
	part, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(part) != 100 {
		t.Fatalf("expected 100 bytes, got %d", len(part))
	}
	for i, b := range part {
		if b != byte((100+i)&0xff) {
			t.Fatalf("byte %d mismatch: want %d, got %d", i, byte(100+i), b)
		}
	}
}

// ---- E2E: Mkdir and Rmdir ----

func TestE2E_MkdirRmdir(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Mkdir
	resp, err := authedHTTPClient.Post(baseURL+"/mkdir?dirname=e2e_testdir", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mkdir expected 200, got %d", resp.StatusCode)
	}

	// Upload a file into the directory
	content := []byte("file in subdir")
	checksum := sha256hex(content)
	status, body := uploadFile(t, baseURL, "e2e_testdir/subfile.txt", content, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload into subdir expected 200, got %d: %s", status, body)
	}

	// List subdir
	resp, err = authedHTTPClient.Get(baseURL + "/api/files?subdir=e2e_testdir")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subdir expected 200, got %d", resp.StatusCode)
	}

	// Rmdir (force remove)
	req, err := http.NewRequest("POST", baseURL+"/rmdir?dirname=e2e_testdir"+"&force=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = authedHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rmdir expected 200, got %d", resp.StatusCode)
	}
}

// ---- E2E: Archive directory ----

func TestE2E_ArchiveDir(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Create the directory first
	mkResp, err := authedHTTPClient.Post(baseURL+"/mkdir?dirname=archivedir", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	mkResp.Body.Close()
	if mkResp.StatusCode != http.StatusOK {
		t.Fatalf("mkdir archivedir expected 200, got %d", mkResp.StatusCode)
	}

	// Prepare files in subdir
	content1 := []byte("archive file 1")
	cs1 := sha256hex(content1)
	status, body := uploadFile(t, baseURL, "archivedir/file1.txt", content1, map[string]string{
		"X-File-Checksum": cs1,
	})
	if status != http.StatusOK {
		t.Fatalf("upload file1 expected 200, got %d: %s", status, body)
	}

	content2 := []byte("archive file 2")
	cs2 := sha256hex(content2)
	status, body = uploadFile(t, baseURL, "archivedir/file2.txt", content2, map[string]string{
		"X-File-Checksum": cs2,
	})
	if status != http.StatusOK {
		t.Fatalf("upload file2 expected 200, got %d: %s", status, body)
	}

	// Archive the directory
	resp, err := authedHTTPClient.Get(baseURL + "/api/archive-dir?dirname=archivedir")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive-dir expected 200, got %d", resp.StatusCode)
	}

	archiveData, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(archiveData) == 0 {
		t.Fatal("archive-dir returned empty body")
	}
}

// ---- E2E: Batch operations ----

func TestE2E_BatchDelete(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Upload two files
	for _, name := range []string{"batch_a.txt", "batch_b.txt"} {
		content := []byte("batch " + name)
		cs := sha256hex(content)
		status, body := uploadFile(t, baseURL, name, content, map[string]string{
			"X-File-Checksum": cs,
		})
		if status != http.StatusOK {
			t.Fatalf("upload %s expected 200, got %d: %s", name, status, body)
		}
	}

	// Delete first file via POST /delete
	checksum := sha256hex([]byte("batch batch_a.txt"))
	reqBody := fmt.Sprintf(`{"files":[{"filename":"batch_a.txt","checksum":"%s"}]}`, checksum)
	req, err := http.NewRequest("POST", baseURL+"/api/batch/delete", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := authedHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("batch-delete response: %s", respBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch-delete expected 200, got %d: %s", resp.StatusCode, respBody)
	}
}

// ---- E2E: sclient CLI commands via subprocess ----

func TestE2E_SclientCLI(t *testing.T) {
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Build sclient binary
	tmpDir := t.TempDir()
	// Isolate XDG_CACHE_HOME to avoid loadCurrentDir() reading user's local cache,
	// which would cause `list` to filter by a stale subdirectory path.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmpDir, "cache"))
	binName := "sclient"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	_ = binPath // sclient binary built but not directly exercised here (upload done via HTTP)
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(currentFile))
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	// sclient upload
	uploadFile(t, baseURL, "sclient_test.txt", []byte("sclient e2e"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("sclient e2e")),
	})

	// sclient list: use a temp config to avoid local tunnel interference
	cfgPath := filepath.Join(tmpDir, "sclient.yaml")
	cfgContent := fmt.Sprintf("server_url: %s\naccess_key: %s\naccess_key_secret: %s\n", baseURL, e2eTestAK, e2eTestSK)
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binPath, "list", "--config", cfgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sclient list: %v\n%s", err, out)
	}
	t.Logf("sclient list: %s", out)
	if !strings.Contains(string(out), "sclient_test.txt") {
		t.Errorf("expected sclient_test.txt in list output, got: %s", out)
	}
}

// ---- E2E: Web UI accessibility ----

func TestE2E_WebUIAccessible(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	resp, err := authedHTTPClient.Get(baseURL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/ failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// ---- E2E: Health endpoint ----

func TestE2E_HealthEndpoint(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	resp, err := authedHTTPClient.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("expected 'OK', got %q", string(body))
	}
}

// ---- E2E: File list after upload ----

func TestE2E_FileListAfterUpload(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Upload a file
	data := []byte("hello e2e list test")
	checksum := sha256hex(data)
	status, body := uploadFile(t, baseURL, "list_test.txt", data, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload failed: %d %s", status, body)
	}

	// List files via /api/files to verify
	resp, err := authedHTTPClient.Get(baseURL + "/api/files")
	if err != nil {
		t.Fatalf("GET /api/files failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list files expected 200, got %d", resp.StatusCode)
	}
	listBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(listBody), "list_test.txt") {
		t.Errorf("expected list_test.txt in file list, got: %s", listBody)
	}
}

// ---- E2E: SproxySig 请求签名认证 ----

// generateTestAccessKeyPair 生成一对 SproxySig AK/SK（AccessKey=ak-<16hex>，Secret=32B hex）。
func generateTestAccessKeyPair(t *testing.T) (string, string) {
	t.Helper()
	akBytes := make([]byte, 16)
	skBytes := make([]byte, 32)
	if _, err := rand.Read(akBytes); err != nil {
		t.Fatalf("generate AK: %v", err)
	}
	if _, err := rand.Read(skBytes); err != nil {
		t.Fatalf("generate SK: %v", err)
	}
	return "ak-" + hex.EncodeToString(akBytes), hex.EncodeToString(skBytes)
}

// parseSeedFromAccessKeysYAML 从 accessKeysYAML 片段（"  - key: ...\n    secret: ...\n"）
// 提取 key/secret，供 pre-seed 凭据 store。无法解析返回空串（调用方按需处理）。
func parseSeedFromAccessKeysYAML(t *testing.T, accessKeysYAML string) (string, string) {
	t.Helper()
	var ak, sk string
	for line := range strings.SplitSeq(accessKeysYAML, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimSpace(line)
		scheme, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(scheme) {
		case "key":
			ak = val
		case "secret":
			sk = val
		}
	}
	if ak == "" || sk == "" {
		t.Fatalf("parseSeedFromAccessKeysYAML: 无法解析 key/secret: %q", accessKeysYAML)
	}
	return ak, sk
}

// startSPROXYWithAccessKeys 启动一个配置了 SproxySig access_keys 的 sproxy（TLS 关闭），
// 返回 baseURL 与 cleanup。accessKeysYAML 形如 "  - key: ...\n    secret: ...\n"。
func startSPROXYWithAccessKeys(t *testing.T, accessKeysYAML string) (string, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	binName := "sproxy"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	moduleRoot := e2eModuleRoot()
	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if buildOut, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, buildOut)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close() //nolint:staticcheck

	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatalf("create uploads dir: %v", err)
	}
	// 凭据 store 化：access_keys 不再装配 Ring，须 pre-seed 使 ak 被识别。
	// accessKeysYAML 参数是可注入的键值片段；这里用解析出的 ak 对应 seed。
	ak, sk := parseSeedFromAccessKeysYAML(t, accessKeysYAML)
	seedCredentialStore(t, uploadsDir, ak, sk)
	configPath := filepath.Join(tmpDir, "sproxy.yaml")
	configContent := "tls:\n  enabled: false\ntunnel_key: \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"\naccess_keys:\n" + accessKeysYAML
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cmd := exec.Command(binPath, "--addr", addr, "--storage-root", uploadsDir, "--config", configPath)
	cmd.Dir = moduleRoot
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy: %v", err)
	}

	baseURL := fmt.Sprintf("http://%s", addr)
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := authedHTTPClient.Get(baseURL + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "OK" {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("sproxy did not become ready; stderr:\n%s", stderrBuf.String())
	}
	return baseURL, newKillWaitCleanup(cmd)
}

// TestE2E_SproxySigAuth 验证 SproxySig 请求签名认证端到端：
//   - 配置 access_keys 的服务端拒绝未签名请求（401）；
//   - sclient 用 --access-key/--access-key-secret 签名后请求成功（list --json 退出 0）。
func TestE2E_SproxySigAuth(t *testing.T) {
	t.Parallel()
	ak, sk := generateTestAccessKeyPair(t)
	baseURL, cleanup := startSPROXYWithAccessKeys(t, "  - key: \""+ak+"\"\n    secret: \""+sk+"\"\n")
	defer cleanup()

	// 未签名 GET /api/files → 401（access_keys 强制验签）
	resp, err := authedHTTPClient.Get(baseURL + "/api/files")
	if err != nil {
		t.Fatalf("unsigned GET /api/files: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned /api/files expected 401, got %d", resp.StatusCode)
	}

	// sclient list（SproxySig 签名）→ 退出码 0（未签名会 401 → 非零退出）。
	// 服务端日志可确认 GET /api/files 被签名请求命中并返回 200。
	bin := e2eBinPath(t, "cmd/sclient")
	args := []string{"list", "--server", baseURL, "--access-key", ak, "--access-key-secret", sk}
	cmd := exec.Command(bin, args...)
	cmd.Dir = e2eModuleRoot()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sclient list (signed) failed: %v\nstderr: %s\nstdout: %s", err, stderr.String(), stdout.String())
	}
}
