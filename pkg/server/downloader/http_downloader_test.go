// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
		w.Header().Set("Content-Range", "bytes 10-44/45")
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

// --- Idle timeout & retryable error tests ---

func TestHTTPDownloader_IdleTimeout_StalledBody(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		// 发送一部分数据后永久停流，等待 idle 超时触发
		_, _ = w.Write([]byte("partial-data"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	dl := &downloader.HTTPDownloader{IdleTimeout: 100 * time.Millisecond}
	dest := filepath.Join(t.TempDir(), "stalled.bin")
	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected idle timeout error")
	}
	var retryable *downloader.RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected RetryableError, got %T: %v", err, err)
	}
}

func TestHTTPDownloader_Status5xx_Retryable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "5xx.bin")
	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected error for 502")
	}
	var retryable *downloader.RetryableError
	if !errors.As(err, &retryable) {
		t.Fatalf("expected RetryableError for 5xx, got %T: %v", err, err)
	}
}

func TestHTTPDownloader_Status4xx_NotRetryable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "4xx.bin")
	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected error for 403")
	}
	var retryable *downloader.RetryableError
	if errors.As(err, &retryable) {
		t.Fatalf("4xx must not be retryable, got %v", err)
	}
}

// --- Range resume edge cases ---

func TestHTTPDownloader_RangeResume_MismatchFallsBackToFull(t *testing.T) {
	t.Parallel()
	fullContent := []byte("the complete and correct full file content")
	partialContent := fullContent[:6]
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Range") == "bytes=6-" {
			// 服务端文件已变化：返回与本地不一致的 Content-Range
			w.Header().Set("Content-Range", "bytes 0-39/40")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("different content from another file"))
			return
		}
		w.Write(fullContent)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "mismatch.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("expected fallback full download to succeed, got %v", err)
	}
	if requests < 2 {
		t.Fatalf("expected at least 2 requests (resume + full fallback), got %d", requests)
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatal("expected partial file removed after mismatch fallback")
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

func TestHTTPDownloader_RangeResume_416FinalizePartial(t *testing.T) {
	t.Parallel()
	fullContent := []byte("already downloaded complete file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(fullContent)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "finalize.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, fullContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("expected 416 finalize to succeed, got %v", err)
	}
	if result.Size != int64(len(fullContent)) {
		t.Fatalf("expected size %d, got %d", len(fullContent), result.Size)
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatal("expected partial renamed away")
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

func TestHTTPDownloader_RangeResume_416FullRedownload(t *testing.T) {
	t.Parallel()
	partialContent := []byte("stale partial data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */100")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "416-redownload.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	_, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err == nil {
		t.Fatal("expected error for 416 full redownload against a server that never returns 200")
	}
}

func TestHTTPDownloader_LargePartialResume_MemorySafe(t *testing.T) {
	t.Parallel()
	// 1 MiB 内容验证流式续传哈希正确（旧实现用 os.ReadFile 会整体读入内存）
	fullContent := make([]byte, 1024*1024)
	for i := range fullContent {
		fullContent[i] = byte(i % 251)
	}
	partialSize := 300 * 1024
	partialContent := fullContent[:partialSize]
	remainingContent := fullContent[partialSize:]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-", partialSize) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", partialSize, len(fullContent)-1, len(fullContent)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(remainingContent)
			return
		}
		w.Write(fullContent)
	}))
	defer srv.Close()

	dl := &downloader.HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "large-resume.bin")
	partialPath := dest + ".partial"
	os.WriteFile(partialPath, partialContent, 0644)

	result, err := dl.Download(t.Context(), srv.URL, dest, nil)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if len(got) != len(fullContent) {
		t.Fatalf("expected %d bytes, got %d", len(fullContent), len(got))
	}
	h := sha256.Sum256(fullContent)
	if result.Checksum != hex.EncodeToString(h[:]) {
		t.Fatalf("checksum mismatch")
	}
	if result.Size != int64(len(fullContent)) {
		t.Fatalf("expected size %d, got %d", len(fullContent), result.Size)
	}
}
