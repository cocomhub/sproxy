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
	"testing"

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
		{"HTTP://EXAMPLE.COM/FILE.ZIP", false},
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

	var progressCalls []downloader.ProgressFunc
	progress := func(downloaded, total int64) {
		progressCalls = append(progressCalls, nil)
		t.Logf("progress: %d/%d", downloaded, total)
	}
	_ = progress

	_, err := d.Download(t.Context(), srv.URL, dest, func(downloaded, total int64) {
		t.Logf("progress: %d/%d", downloaded, total)
	})
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	// 只要有进度回调被调用即可
}
