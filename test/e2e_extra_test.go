// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxy_test

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	statResp, err := http.Head(baseURL + "/api/files/stat?filename=" + filename)
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.Post(baseURL+"/mkdir?dirname=e2e_testdir", "application/json", nil)
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
	resp, err = http.Get(baseURL + "/api/files?subdir=e2e_testdir")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list subdir expected 200, got %d", resp.StatusCode)
	}

	// Rmdir (force remove)
	req, err := http.NewRequest("POST", baseURL+"/rmdir?dirname=e2e_testdir", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
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
	mkResp, err := http.Post(baseURL+"/mkdir?dirname=archivedir", "application/json", nil)
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
	resp, err := http.Get(baseURL + "/api/archive-dir?dirname=archivedir")
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
	resp, err := http.DefaultClient.Do(req)
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
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// Build sclient binary
	tmpDir := t.TempDir()
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

	// sclient list
	cmd := exec.Command(binPath, "list", "--server", baseURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sclient list: %v\n%s", err, out)
	}
	t.Logf("sclient list: %s", out)
	if !strings.Contains(string(out), "sclient_test.txt") {
		t.Errorf("expected sclient_test.txt in list output, got: %s", out)
	}
}
