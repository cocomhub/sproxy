// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestCloudDownloadCmd_UseAndArgs(t *testing.T) {
	if cloudDownloadCmd.Use != "cloud-download <url>" {
		t.Fatalf("expected Use 'cloud-download <url>', got %q", cloudDownloadCmd.Use)
	}
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
