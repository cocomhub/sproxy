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

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestCloudDownloadCmd_UseAndArgs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)
	if cmd.Use != "cloud-download <url> [url...]" {
		t.Fatalf("expected Use 'cloud-download <url> [url...]', got %q", cmd.Use)
	}
	// Args 应为非 nil（ArbitraryArgs 是有效的验证函数）
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

func TestCloudDownloadCmd_CreateTask(t *testing.T) {
	content := []byte("hello")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-test-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_AsyncPolling(t *testing.T) {
	pollCount := 0
	content := []byte("downloaded content")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--poll-interval", "100ms", "https://example.com/large.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-async-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_TaskFailed(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--poll-interval", "100ms", "https://example.com/fail.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when task fails")
	}
}

func TestCloudDownloadCmd_ChecksumMismatch(t *testing.T) {
	content := []byte("content")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "https://example.com/file.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when checksum mismatch")
	}
}

func TestCloudDownloadCmd_NoCleanupFlag(t *testing.T) {
	deletedCloud := false
	content := []byte("content")
	chk := sha256.Sum256(content)
	correctChecksum := hex.EncodeToString(chk[:])

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outPath := filepath.Join(t.TempDir(), "file.zip")
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--no-cleanup", "--output", outPath, "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if deletedCloud {
		t.Fatal("expected no cloud delete with --no-cleanup flag")
	}
}

func TestCloudDownloadCmd_ForceAsync(t *testing.T) {
	content := []byte("data")
	chk := sha256.Sum256(content)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outPath := filepath.Join(t.TempDir(), "small.zip")
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--force-async", "--poll-interval", "100ms", "--output", outPath, "https://example.com/small.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-forceasync-1") {
		t.Fatalf("expected download completion message, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_OutputFlag(t *testing.T) {
	content := []byte("output content")
	chk := sha256.Sum256(content)
	correctChecksum := hex.EncodeToString(chk[:])

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outPath := filepath.Join(t.TempDir(), "custom-name.bin")
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--output", outPath, "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
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
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "https://example.com/a.zip") {
		t.Fatalf("expected output to contain first URL, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "https://example.com/b.zip") {
		t.Fatalf("expected output to contain second URL, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_BatchFileFlag(t *testing.T) {
	content := []byte("batch content")
	chk := sha256.Sum256(content)

	batchFile := filepath.Join(t.TempDir(), "batch-urls.txt")
	os.WriteFile(batchFile, []byte("https://example.com/batch-file.zip\n"), 0644)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download" && r.Method == http.MethodPost {
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "--batch", batchFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-batch-file-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
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
			task := map[string]any{
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.PersistentFlags().String("output", "", "")
	cmd.SetArgs([]string{"--server", mock.URL, "https://example.com/good.zip", "https://example.com/bad.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when partial failure")
	}
}
