// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

func TestHTTPDownloader_SupportsHTTPSchemes(t *testing.T) {
	d := &downloader.HTTPDownloader{}
	tests := []struct {
		url      string
		expected bool
	}{
		{"http://example.com/file.zip", true},
		{"https://example.com/file.zip", true},
		{"HTTP://EXAMPLE.COM/FILE.ZIP", true},
		{"ftp://example.com/file.zip", false},
		{"", false},
		{"file:///tmp/file.zip", false},
	}
	for _, tt := range tests {
		if got := d.Supports(tt.url); got != tt.expected {
			t.Errorf("Supports(%q) = %v, want %v", tt.url, got, tt.expected)
		}
	}
}

func TestHTTPDownloader_Name(t *testing.T) {
	d := &downloader.HTTPDownloader{}
	if d.Name() != "http" {
		t.Fatalf("expected 'http', got %q", d.Name())
	}
}

func TestHTTPDownloader_Download_Success(t *testing.T) {
	content := []byte("hello world from test server")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	d := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "downloaded.bin")

	result, err := d.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}

	// 校验文件内容
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("expected %q, got %q", string(content), string(got))
	}

	// 校验 checksum
	h := sha256.Sum256(content)
	expectedChecksum := hex.EncodeToString(h[:])
	if result.Checksum != expectedChecksum {
		t.Fatalf("expected checksum %s, got %s", expectedChecksum, result.Checksum)
	}
	if result.Size != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), result.Size)
	}
}

func TestHTTPDownloader_Download_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "notfound.bin")

	_, err := d.Download(t.Context(), srv.URL+"/missing", dest, nil)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestHTTPDownloader_Download_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 缓慢响应，给取消留时间
		w.WriteHeader(http.StatusOK)
		for range 100 {
			w.Write([]byte("data"))
		}
	}))
	defer srv.Close()

	d := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "cancelled.bin")

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 立即取消

	_, err := d.Download(ctx, srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestHTTPDownloader_Download_Progress(t *testing.T) {
	content := make([]byte, 1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.Write(content)
	}))
	defer srv.Close()

	d := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "progress.bin")

	var progressCount atomic.Int32
	_, err := d.Download(t.Context(), srv.URL, dest, func(downloaded, total int64) {
		progressCount.Add(1)
		_ = downloaded
		_ = total
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if progressCount.Load() == 0 {
		t.Fatal("expected progress callback to be called at least once")
	}
}

func TestHTTPDownloader_PreservesMTime(t *testing.T) {
	t.Parallel()
	expectedTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", expectedTime.Format(time.RFC1123))
		w.Write([]byte("test content"))
	}))
	defer ts.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "output.txt")
	result, err := dl.Download(t.Context(), ts.URL, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ModTime.Equal(expectedTime) {
		t.Errorf("expected ModTime %v, got %v", expectedTime, result.ModTime)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(expectedTime) {
		t.Errorf("expected file mtime %v, got %v", expectedTime, info.ModTime())
	}
}

func TestHTTPDownloader_NoLastModified(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("no last-modified header"))
	}))
	defer ts.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "output.txt")
	result, err := dl.Download(t.Context(), ts.URL, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ModTime.IsZero() {
		t.Errorf("expected zero ModTime, got %v", result.ModTime)
	}
}

// --- Timeout tests ---

func TestHTTPDownloader_Timeout_Exceeded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte("slow response"))
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{
		Timeout: 1 * time.Millisecond,
	}
	dest := filepath.Join(t.TempDir(), "timeout.bin")
	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestHTTPDownloader_Timeout_Zero(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("normal response"))
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "no-timeout.bin")
	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Range resume tests ---

func TestHTTPDownloader_RangeResume_NoPartialFile(t *testing.T) {
	t.Parallel()
	content := []byte("full file content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "no-partial.bin")
	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(content) {
		t.Fatalf("expected %q, got %q", string(content), string(got))
	}
	h := sha256.Sum256(content)
	if result.Checksum != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum mismatch")
	}
}

func TestHTTPDownloader_RangeResume_EmptyPartialFile(t *testing.T) {
	t.Parallel()
	content := []byte("full content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "empty-partial.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, []byte{}, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(content) {
		t.Fatalf("expected %q, got %q", string(content), string(got))
	}
	h := sha256.Sum256(content)
	if result.Checksum != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum mismatch")
	}
}

func TestHTTPDownloader_RangeResume_Append(t *testing.T) {
	t.Parallel()
	fullContent := []byte("hello world this is the complete file content")
	partialContent := fullContent[:10] // first 10 bytes
	remainingContent := fullContent[10:]

	partialFileCreated := false
	_ = partialFileCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			// First request (no partial file yet) — unlikely but handle it
			w.Write(fullContent)
			return
		}
		// Verify Range header
		if rangeHeader != "bytes=10-" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", "bytes 10-49/50")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(remainingContent)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "resume-append.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(fullContent) {
		t.Fatalf("expected %q, got %q", string(fullContent), string(got))
	}
	h := sha256.Sum256(fullContent)
	if result.Checksum != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum mismatch: expected %s, got %s", hex.EncodeToString(h[:]), result.Checksum)
	}
	if result.Size != int64(len(fullContent)) {
		t.Fatalf("expected size %d, got %d", len(fullContent), result.Size)
	}
}

func TestHTTPDownloader_RangeResume_NoRangeSupport(t *testing.T) {
	t.Parallel()
	fullContent := []byte("server does not support range, always returns full content")
	partialContent := fullContent[:15]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always returns 200 OK, ignores Range header
		w.Write(fullContent)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "resume-norange.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	// Partial file should have been removed (no .partial file left)
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatal("expected partial file to be removed after full download fallback")
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(fullContent) {
		t.Fatalf("expected %q, got %q", string(fullContent), string(got))
	}
	h := sha256.Sum256(fullContent)
	if result.Checksum != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum mismatch")
	}
}

func TestHTTPDownloader_RangeResume_ChecksumCorrect(t *testing.T) {
	t.Parallel()
	// Create large content to ensure checksum accumulates correctly
	fullContent := make([]byte, 10000)
	for i := range fullContent {
		fullContent[i] = byte(i % 256)
	}
	partialContent := fullContent[:4000]
	remainingContent := fullContent[4000:]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=4000-" {
			w.Header().Set("Content-Range", "bytes 4000-9999/10000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(remainingContent)
			return
		}
		w.Write(fullContent)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "resume-checksum.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(fullContent) {
		t.Fatalf("expected %q, got %q", string(fullContent), string(got))
	}
	h := sha256.Sum256(fullContent)
	expectedChecksum := hex.EncodeToString(h[:])
	if result.Checksum != expectedChecksum {
		t.Fatalf("checksum mismatch: expected %s, got %s", expectedChecksum, result.Checksum)
	}
	if result.Size != int64(len(fullContent)) {
		t.Fatalf("expected size %d, got %d", len(fullContent), result.Size)
	}
}
