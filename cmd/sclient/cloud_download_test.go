// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

func TestCloudDownloadCmd_BasicBatch(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-batch-1",
						"url":        "https://example.com/file.zip",
						"filename":   "file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-batch-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitCompleted(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-wait-1",
						"url":        "https://example.com/file.zip",
						"filename":   "file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--wait", "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-wait-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitPolling(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-poll-1",
						"url":        "https://example.com/large.zip",
						"filename":   "large.zip",
						"status":     "pending",
						"total_size": 50 * 1024 * 1024,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-poll-1") && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "completed"
			}
			task := map[string]any{
				"id":         "cloud-poll-1",
				"url":        "https://example.com/large.zip",
				"filename":   "large.zip",
				"status":     status,
				"total_size": 50 * 1024 * 1024,
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
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--wait", "--poll-interval", "100ms", "https://example.com/large.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-poll-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "完成") {
		t.Fatalf("expected completion message in output, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitArchiveDownload(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-arch-1",
						"url":        "https://example.com/file.zip",
						"filename":   "file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-arch-1/archive") && r.Method == http.MethodPost {
			result := map[string]any{
				"success": true,
				"file":    "archive.tar.gz",
				"size":    200,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/download") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("archive content"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-arch-1") && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--wait", "--archive", "--archive-name", "myarchive",
		"--download", "--output-dir", outDir,
		"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "archive.tar.gz") {
		t.Fatalf("expected output to contain archive name, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_NoURLs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no URLs provided")
	}
}

func TestCloudDownloadCmd_ReadURLsFromFile(t *testing.T) {
	dir := t.TempDir()

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

	f2 := filepath.Join(dir, "with-comments.txt")
	os.WriteFile(f2, []byte("# comment\n\nhttps://example.com/valid.zip\n  # another comment\n"), 0644)
	urls, err = readURLsFromFile(f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}

	f3 := filepath.Join(dir, "empty.txt")
	os.WriteFile(f3, []byte(""), 0644)
	urls, err = readURLsFromFile(f3)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 0 {
		t.Fatalf("expected 0 URLs, got %d", len(urls))
	}

	_, err = readURLsFromFile(filepath.Join(dir, "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCloudDownloadCmd_BatchFileFlag(t *testing.T) {
	batchFile := filepath.Join(t.TempDir(), "batch-urls.txt")
	os.WriteFile(batchFile, []byte("https://example.com/batch-file.zip\n"), 0644)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-batch-file-1",
						"url":        "https://example.com/batch-file.zip",
						"filename":   "batch-file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--batch", batchFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-batch-file-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestWaitForCompletion_AllCompleted(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	tasks := []client.CloudTask{
		{ID: "t1", Status: "completed", Filename: "f1.zip", TotalSize: 100},
		{ID: "t2", Status: "completed", Filename: "f2.zip", TotalSize: 200},
	}

	svc := client.NewFileClient("http://test.local")
	result, err := waitForCompletion(t.Context(), svc, ios, tasks, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForCompletion failed: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
}

func TestWaitForCompletion_CancelledContext(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	tasks := []client.CloudTask{
		{ID: "t1", Status: "pending", Filename: "f1.zip"},
	}

	svc := client.NewFileClient("http://test.local")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := waitForCompletion(ctx, svc, ios, tasks, 100*time.Millisecond)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("hello world")
	hdr := &tar.Header{
		Name:     "testfile.txt",
		Size:     int64(len(content)),
		Mode:     0644,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	src := filepath.Join(dir, "test.tar.gz")
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, dir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "testfile.txt"))
	if err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "../../../etc/passwd",
		Size:     4,
		Mode:     0644,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	src := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, dir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "evil.tar.gz" {
			continue
		}
		t.Fatalf("unexpected file: %s (path traversal may have succeeded)", e.Name())
	}
}

func TestCloudDownloadCmd_NoCleanup(t *testing.T) {
	deleted := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-noclean-1",
						"url":        "https://example.com/file.zip",
						"filename":   "file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-noclean-1") && r.Method == http.MethodDelete {
			deleted = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--wait", "--no-cleanup", "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if deleted {
		t.Fatal("expected no delete with --no-cleanup flag")
	}
}

func TestCloudDownloadCmd_NewFlags(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	flagNames := []string{"wait", "archive", "archive-name", "download", "output-dir", "extract"}
	for _, name := range flagNames {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected flag --%s to exist", name)
		}
	}
}

func TestCloudDownloadCmd_TaskFailed(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-fail-1",
						"url":        "https://example.com/fail.zip",
						"filename":   "fail.zip",
						"status":     "pending",
						"total_size": 50 * 1024 * 1024,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-fail-1") && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "failed"
			}
			task := map[string]any{
				"id":       "cloud-fail-1",
				"url":      "https://example.com/fail.zip",
				"filename": "fail.zip",
				"status":   status,
				"error":    "connection refused",
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
	cmd.SetArgs([]string{"--wait", "--poll-interval", "100ms", "https://example.com/fail.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when task fails")
	}
}

func TestCloudDownloadCmd_MultipleURLs(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-multi-1",
						"url":        "https://example.com/a.zip",
						"filename":   "a.zip",
						"status":     "completed",
						"total_size": 100,
					},
					{
						"id":         "cloud-multi-2",
						"url":        "https://example.com/b.zip",
						"filename":   "b.zip",
						"status":     "completed",
						"total_size": 200,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "cloud-multi-1") {
		t.Fatalf("expected output to contain first task ID, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "cloud-multi-2") {
		t.Fatalf("expected output to contain second task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_PartialFailure(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "cloud-partial-1",
						"url":        "https://example.com/bad.zip",
						"filename":   "bad.zip",
						"status":     "pending",
						"total_size": 50 * 1024 * 1024,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cloud-partial-1") && r.Method == http.MethodGet {
			task := map[string]any{
				"id":       "cloud-partial-1",
				"url":      "https://example.com/bad.zip",
				"filename": "bad.zip",
				"status":   "failed",
				"error":    "download failed",
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
	cmd.SetArgs([]string{"--wait", "--poll-interval", "100ms", "https://example.com/bad.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when all tasks fail")
	}
}
