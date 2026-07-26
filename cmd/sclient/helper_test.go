// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- readURLsFromFile ----

func TestReadURLsFromFile(t *testing.T) {
	t.Parallel()

	t.Run("normal_lines", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "urls.txt")
		if err := os.WriteFile(f, []byte("https://example.com/file1\nhttps://example.com/file2\n"), 0644); err != nil {
			t.Fatal(err)
		}
		urls, err := readURLsFromFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(urls) != 2 {
			t.Fatalf("expected 2 URLs, got %d", len(urls))
		}
		if urls[0] != "https://example.com/file1" {
			t.Errorf("expected first URL, got %q", urls[0])
		}
	})

	t.Run("skips_comments_and_empty", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "urls2.txt")
		if err := os.WriteFile(f, []byte("# comment\n\nhttps://example.com/file1\n  \n"), 0644); err != nil {
			t.Fatal(err)
		}
		urls, err := readURLsFromFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(urls) != 1 {
			t.Fatalf("expected 1 URL, got %d", len(urls))
		}
	})

	t.Run("file_not_found", func(t *testing.T) {
		_, err := readURLsFromFile("/nonexistent/file.txt")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})
}

// ---- filepathSafe ----

func TestFilepathSafe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"normal.txt", "normal.txt"},
		{"with/path/sep", "with_path_sep"},
		{"with\\backslash", "with_backslash"},
		{"  spaces  ", "spaces"},
		{"  .hidden", "hidden"},
		{"", "download"},
		{"/../../etc/passwd", "_.._.._etc_passwd"},
		{"onlydots...", "onlydots"},
		{"  .  ", "download"},
	}
	for _, tt := range tests {
		got := filepathSafe(tt.input)
		if got != tt.expected {
			t.Errorf("filepathSafe(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ---- isImageExt ----

func TestIsImageExt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ext      string
		expected bool
	}{
		{".png", true},
		{".jpg", true},
		{".jpeg", true},
		{".gif", true},
		{".bmp", true},
		{".webp", true},
		{".svg", true},
		{".txt", false},
		{".go", false},
		{"", false},
		{".pdf", false},
		{".PNG", false}, // 区分大小写
	}
	for _, tt := range tests {
		got := isImageExt(tt.ext)
		if got != tt.expected {
			t.Errorf("isImageExt(%q) = %v, want %v", tt.ext, got, tt.expected)
		}
	}
}

// ---- isTextExt ----

func TestIsTextExt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ext      string
		expected bool
	}{
		{".txt", true},
		{".md", true},
		{".json", true},
		{".yaml", true},
		{".go", true},
		{".py", true},
		{".png", false},
		{".pdf", false},
		{".zip", false},
	}
	for _, tt := range tests {
		got := isTextExt(tt.ext)
		if got != tt.expected {
			t.Errorf("isTextExt(%q) = %v, want %v", tt.ext, got, tt.expected)
		}
	}
}

// ---- buildTunnelRequest ----

func TestBuildTunnelRequest(t *testing.T) {
	t.Parallel()

	t.Run("basic_get", func(t *testing.T) {
		req, err := buildTunnelRequest(tunnelReqOpts{method: "GET", targetURL: "http://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if req.Method != "GET" {
			t.Errorf("expected GET, got %s", req.Method)
		}
	})

	t.Run("with_body", func(t *testing.T) {
		req, err := buildTunnelRequest(tunnelReqOpts{method: "POST", targetURL: "http://example.com", body: "hello"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := req.GetBody()
		if body == nil {
			t.Error("expected non-nil GetBody")
		}
	})

	t.Run("with_headers", func(t *testing.T) {
		req, err := buildTunnelRequest(tunnelReqOpts{
			method:    "GET",
			targetURL: "http://example.com",
			headers:   []string{"X-Custom: value1", "Authorization: Bearer token"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("X-Custom") != "value1" {
			t.Errorf("expected X-Custom: value1, got %s", req.Header.Get("X-Custom"))
		}
		if req.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("expected Authorization: Bearer token, got %s", req.Header.Get("Authorization"))
		}
	})

	t.Run("invalid_url", func(t *testing.T) {
		_, err := buildTunnelRequest(tunnelReqOpts{method: "GET", targetURL: "://invalid"})
		if err == nil {
			t.Error("expected error for invalid URL")
		}
	})
}

// ---- resolveOutputPath ----

func TestResolveOutputPath(t *testing.T) {
	t.Parallel()

	t.Run("with_output_file", func(t *testing.T) {
		got, err := resolveOutputPath("http://example.com/file.zip", "/tmp/out.zip", "")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/tmp/out.zip" {
			t.Errorf("expected /tmp/out.zip, got %s", got)
		}
	})

	t.Run("without_output_file", func(t *testing.T) {
		got, err := resolveOutputPath("http://example.com/file.zip", "", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(got, "file.zip") {
			t.Errorf("expected path to end with file.zip, got %s", got)
		}
	})

	t.Run("invalid_url", func(t *testing.T) {
		_, err := resolveOutputPath("://invalid", "", "")
		if err == nil {
			t.Error("expected error for invalid URL")
		}
	})
}

// ---- writeWithProgress ----

func TestWriteWithProgress(t *testing.T) {
	content := []byte("hello world")
	var wBuf strings.Builder
	n, err := writeWithProgress(
		strings.NewReader(string(content)),
		&wBuf,
		int64(len(content)),
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(content)) {
		t.Errorf("expected %d bytes, got %d", len(content), n)
	}
	if wBuf.String() != string(content) {
		t.Errorf("expected %q, got %q", string(content), wBuf.String())
	}
}

func TestWriteWithProgress_NoLength(t *testing.T) {
	content := []byte("hello")
	var wBuf strings.Builder
	n, err := writeWithProgress(
		strings.NewReader(string(content)),
		&wBuf,
		-1,
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(content)) {
		t.Errorf("expected %d bytes, got %d", len(content), n)
	}
}
