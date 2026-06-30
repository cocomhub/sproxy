// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestCloudDownloadCmd_UseAndArgs(t *testing.T) {
	if cloudDownloadCmd.Use != "cloud-download <url> [url...]" {
		t.Fatalf("expected Use 'cloud-download <url> [url...]', got %q", cloudDownloadCmd.Use)
	}
	// Args 应为非 nil（ArbitraryArgs 是有效的验证函数）
	if cloudDownloadCmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

func TestCloudDownloadCmd_CreateTask(t *testing.T) {
	content := []byte("hello")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-test-1",
				"url":        "https://example.com/file.zip",
				"filename":   "file.zip",
				"status":     "completed",
				"total_size": int64(len(content)),
				"checksum":   hex.EncodeToString(chk[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk[:]))
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "https://example.com/file.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "cloud-test-1") {
		t.Fatalf("expected output to contain task ID, got: %s", out)
	}
}

func TestCloudDownloadCmd_AsyncPolling(t *testing.T) {
	pollCount := 0
	content := []byte("downloaded content")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-async-1",
				"url":        "https://example.com/large.zip",
				"filename":   "large.zip",
				"status":     "pending",
				"total_size": 50 * 1024 * 1024,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-async-1") {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "completed"
			}
			task := map[string]interface{}{
				"id":         "cloud-async-1",
				"url":        "https://example.com/large.zip",
				"filename":   "large.zip",
				"status":     status,
				"total_size": 50 * 1024 * 1024,
				"checksum":   hex.EncodeToString(chk[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk[:]))
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--poll-interval", "100ms", "https://example.com/large.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "cloud-async-1") {
		t.Fatalf("expected output to contain task ID, got: %s", out)
	}
}

func TestCloudDownloadCmd_TaskFailed(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-fail-1",
				"url":        "https://example.com/fail.zip",
				"filename":   "fail.zip",
				"status":     "pending",
				"total_size": 50 * 1024 * 1024,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-fail-1") {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "failed"
			}
			task := map[string]interface{}{
				"id":         "cloud-fail-1",
				"url":        "https://example.com/fail.zip",
				"filename":   "fail.zip",
				"status":     status,
				"total_size": 50 * 1024 * 1024,
				"error":      "connection refused",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStderr(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--poll-interval", "100ms", "https://example.com/fail.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "failed") && !strings.Contains(out, "失败") {
		t.Fatalf("expected error output about failed task, got: %s", out)
	}
}

func TestCloudDownloadCmd_ChecksumMismatch(t *testing.T) {
	content := []byte("content")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-chk-1",
				"url":        "https://example.com/file.zip",
				"filename":   "file.zip",
				"status":     "completed",
				"checksum":   "wrongchecksum",
				"total_size": int64(len(content)),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", "wrongchecksum")
			w.Write(content)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStderr(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "https://example.com/file.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "checksum") && !strings.Contains(out, "校验") {
		t.Fatalf("expected checksum mismatch error, got: %s", out)
	}
}

func TestCloudDownloadCmd_NoCleanupFlag(t *testing.T) {
	deletedCloud := false
	content := []byte("content")
	chk := sha256.Sum256(content)
	correctChecksum := hex.EncodeToString(chk[:])

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-noclean-1",
				"url":        "https://example.com/file.zip",
				"filename":   "file.zip",
				"status":     "completed",
				"checksum":   correctChecksum,
				"total_size": int64(len(content)),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", correctChecksum)
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" && r.Method == http.MethodPost {
			deletedCloud = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	outPath := filepath.Join(t.TempDir(), "file.zip")
	testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--no-cleanup", "--output", outPath, "https://example.com/file.zip"})
		rootCmd.Execute()
	})

	if deletedCloud {
		t.Fatal("expected no cloud delete with --no-cleanup flag")
	}
}

func TestCloudDownloadCmd_ForceAsync(t *testing.T) {
	content := []byte("data")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-forceasync-1",
				"url":        "https://example.com/small.zip",
				"filename":   "small.zip",
				"status":     "pending",
				"total_size": int64(len(content)),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-forceasync-1") {
			task := map[string]interface{}{
				"id":         "cloud-forceasync-1",
				"url":        "https://example.com/small.zip",
				"filename":   "small.zip",
				"status":     "completed",
				"total_size": int64(len(content)),
				"checksum":   hex.EncodeToString(chk[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk[:]))
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	outPath := filepath.Join(t.TempDir(), "small.zip")
	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--force-async", "--poll-interval", "100ms", "--output", outPath, "https://example.com/small.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "Downloaded") && !strings.Contains(out, "下载") {
		t.Fatalf("expected download completion message, got: %s", out)
	}
}

func TestCloudDownloadCmd_OutputFlag(t *testing.T) {
	content := []byte("output content")
	chk := sha256.Sum256(content)
	correctChecksum := hex.EncodeToString(chk[:])

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-out-1",
				"url":        "https://example.com/file.zip",
				"filename":   "file.zip",
				"status":     "completed",
				"checksum":   correctChecksum,
				"total_size": int64(len(content)),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", correctChecksum)
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	outPath := filepath.Join(t.TempDir(), "custom-name.bin")
	testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--output", outPath, "https://example.com/file.zip"})
		rootCmd.Execute()
	})

	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected file at %s to exist: %v", outPath, err)
	}
}

func TestCloudDownloadCmd_ReadURLsFromFile(t *testing.T) {
	dir := t.TempDir()

	// 正常文件
	f1 := filepath.Join(dir, "urls.txt")
	os.WriteFile(f1, []byte("https://example.com/a.zip\nhttps://example.com/b.zip\n"), 0644)
	urls, err := readURLsFromFile(f1)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	if urls[0] != "https://example.com/a.zip" {
		t.Fatalf("expected first URL, got %q", urls[0])
	}

	// 含注释和空行的文件
	f2 := filepath.Join(dir, "with-comments.txt")
	os.WriteFile(f2, []byte("# comment\n\nhttps://example.com/valid.zip\n  # another comment\n"), 0644)
	urls, err = readURLsFromFile(f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}

	// 空文件
	f3 := filepath.Join(dir, "empty.txt")
	os.WriteFile(f3, []byte(""), 0644)
	urls, err = readURLsFromFile(f3)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 0 {
		t.Fatalf("expected 0 URLs, got %d", len(urls))
	}

	// 文件不存在
	_, err = readURLsFromFile(filepath.Join(dir, "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCloudDownloadCmd_MultipleURLs(t *testing.T) {
	content1 := []byte("file1")
	chk1 := sha256.Sum256(content1)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(io.NopCloser(r.Body)).Decode(&body)
			task := map[string]interface{}{
				"id":         "cloud-multi-" + body.URL[len(body.URL)-1:],
				"url":        body.URL,
				"filename":   "file" + body.URL[len(body.URL)-1:] + ".zip",
				"status":     "completed",
				"total_size": int64(len(content1)),
				"checksum":   hex.EncodeToString(chk1[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk1[:]))
			w.Write(content1)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		// 重置 --output flag 避免上一测试的状态泄漏
		rootCmd.PersistentFlags().Set("output", "")
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "https://example.com/a.zip", "https://example.com/b.zip"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "https://example.com/a.zip") {
		t.Fatalf("expected output to contain first URL, got: %s", out)
	}
	if !strings.Contains(out, "https://example.com/b.zip") {
		t.Fatalf("expected output to contain second URL, got: %s", out)
	}
}

func TestCloudDownloadCmd_BatchFileFlag(t *testing.T) {
	content := []byte("batch content")
	chk := sha256.Sum256(content)

	batchFile := filepath.Join(t.TempDir(), "batch-urls.txt")
	os.WriteFile(batchFile, []byte("https://example.com/batch-file.zip\n"), 0644)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]interface{}{
				"id":         "cloud-batch-file-1",
				"url":        "https://example.com/batch-file.zip",
				"filename":   "batch-file.zip",
				"status":     "completed",
				"total_size": int64(len(content)),
				"checksum":   hex.EncodeToString(chk[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk[:]))
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "--batch", batchFile})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "cloud-batch-file-1") {
		t.Fatalf("expected output to contain task ID, got: %s", out)
	}
}

func TestCloudDownloadCmd_PartialFailure(t *testing.T) {
	content := []byte("partial content")
	chk := sha256.Sum256(content)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(io.NopCloser(r.Body)).Decode(&body)
			if body.URL == "https://example.com/bad.zip" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "bad URL"})
				return
			}
			task := map[string]interface{}{
				"id":         "cloud-partial-1",
				"url":        body.URL,
				"filename":   "partial.zip",
				"status":     "completed",
				"total_size": int64(len(content)),
				"checksum":   hex.EncodeToString(chk[:]),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("X-File-Checksum", hex.EncodeToString(chk[:]))
			w.Write(content)
			return
		}
		if r.URL.Path == "/delete" || strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	stderrOut := testutil.CaptureStderr(func() {
		testutil.CaptureStdout(func() {
			rootCmd.SetArgs([]string{"cloud-download", "--server", mock.URL, "https://example.com/good.zip", "https://example.com/bad.zip"})
			rootCmd.Execute()
		})
	})

	// 应该包含失败信息
	if !strings.Contains(stderrOut, "bad") && !strings.Contains(stderrOut, "失败") {
		t.Fatalf("expected stderr to contain failure info, got: %s", stderrOut)
	}
}
