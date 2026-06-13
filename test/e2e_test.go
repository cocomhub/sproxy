// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxy_test provides end-to-end smoke tests for the sproxy server.
// Tests build and start a real sproxy binary, then exercise its HTTP API.
package sproxy_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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
	if _, err := part.Write(body); err != nil {
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
	if err := json.Unmarshal(body, &result); err != nil {
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
	args := []string{
		"--addr", addr,
		"--uploads-dir", uploadsDir,
		"--config", filepath.Join(tmpDir, "nonexistent.yaml"),
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
