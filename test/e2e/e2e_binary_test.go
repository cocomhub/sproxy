// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e_test

import (
	"bytes"
	"fmt"
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

// TestE2E_Binary tests the full build -> start -> upload -> list -> download -> delete
// workflow using the built sproxy and sclient binaries.
func TestE2E_Binary_UploadDownloadDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e binary test in short mode")
	}

	tmpDir := t.TempDir()

	// Locate module root: test/e2e/e2e_binary_test.go -> test/e2e/ -> test/ -> module root
	_, currentFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(currentFile)))

	binDir := filepath.Join(tmpDir, "bin")
	uploadsDir := filepath.Join(tmpDir, "uploads")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// ---- Build sproxy binary ----
	t.Log("Building sproxy...")
	sproxyBin := filepath.Join(binDir, "sproxy")
	if runtime.GOOS == "windows" {
		sproxyBin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", sproxyBin, "./cmd/sproxy")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sproxy: %v\n%s", err, out)
	}

	// ---- Build sclient binary ----
	t.Log("Building sclient...")
	sclientBin := filepath.Join(binDir, "sclient")
	if runtime.GOOS == "windows" {
		sclientBin += ".exe"
	}
	buildCmd = exec.Command("go", "build", "-o", sclientBin, "./cmd/sclient")
	buildCmd.Dir = moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build sclient: %v\n%s", err, out)
	}

	// ---- Find a free port ----
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close() // close immediately; race with sproxy is acceptable for tests
	t.Logf("Using address: %s", addr)

	// ---- Start sproxy ----
	args := []string{
		"--addr", addr,
		"--uploads-dir", uploadsDir,
	}
	cmd := exec.Command(sproxyBin, args...)
	cmd.Dir = moduleRoot

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sproxy: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	// ---- Wait for server readiness ----
	baseURL := fmt.Sprintf("http://%s", addr)
	healthOK := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				healthOK = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthOK {
		t.Fatalf("server at %s did not become ready within 5s\nstdout: %s\nstderr: %s",
			addr, stdoutBuf.String(), stderrBuf.String())
	}
	t.Logf("Server ready at %s", baseURL)

	// ---- Create test file ----
	testContent := "hello e2e binary test"
	if err := os.WriteFile(filepath.Join(tmpDir, "hello.txt"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	// ---- Upload via sclient ----
	// Run sclient from tmpDir so the local file path "hello.txt" resolves to
	// a simple remote filename "hello.txt" (not a Windows absolute path).
	t.Log("Uploading file via sclient...")
	uploadCmd := exec.Command(sclientBin, "--server="+baseURL, "upload", "hello.txt")
	uploadCmd.Dir = tmpDir
	out, err := uploadCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("upload failed: %v\n%s", err, out)
	}
	t.Logf("Upload output: %s", strings.TrimSpace(string(out)))

	// ---- List via sclient ----
	t.Log("Listing files via sclient...")
	listCmd := exec.Command(sclientBin, "--server="+baseURL, "list")
	listCmd.Dir = tmpDir
	out, err = listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	t.Logf("List output:\n%s", strings.TrimSpace(string(out)))
	if !strings.Contains(string(out), "hello.txt") {
		t.Errorf("list output should contain hello.txt, got: %s", out)
	}

	// ---- Download via sclient ----
	t.Log("Downloading file via sclient...")
	dstFile := filepath.Join(tmpDir, "downloaded.txt")
	dlCmd := exec.Command(sclientBin, "--server="+baseURL, "download", "hello.txt", dstFile)
	dlCmd.Dir = tmpDir
	out, err = dlCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("download failed: %v\n%s", err, out)
	}
	t.Logf("Download output: %s", strings.TrimSpace(string(out)))

	// Verify downloaded content
	downloaded, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != testContent {
		t.Errorf("downloaded content mismatch: got %q, want %q", string(downloaded), testContent)
	}
	t.Log("Downloaded content verified")

	// ---- Delete via sclient ----
	t.Log("Deleting file via sclient...")
	delCmd := exec.Command(sclientBin, "--server="+baseURL, "delete", "hello.txt")
	delCmd.Dir = tmpDir
	out, err = delCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("delete failed: %v\n%s", err, out)
	}
	t.Logf("Delete output: %s", strings.TrimSpace(string(out)))
}
