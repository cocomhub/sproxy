// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientArchive_SingleFile(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/archive" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		// 验证请求体包含 {"files":["test.txt"]}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		var req struct {
			Files []string `json:"files"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "json error", http.StatusBadRequest)
			return
		}
		if len(req.Files) != 1 || req.Files[0] != "test.txt" {
			http.Error(w, "unexpected files", http.StatusBadRequest)
			return
		}
		tw := tar.NewWriter(w)
		tw.WriteHeader(&tar.Header{
			Name: "test.txt",
			Size: 4,
		})
		tw.Write([]byte("data"))
		tw.Close()
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	dst := filepath.Join(t.TempDir(), "out.tar")

	err := c.Archive(t.Context(), []string{"test.txt"}, dst)
	if err != nil {
		t.Fatalf("Archive() = %v", err)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("archive file is empty")
	}

	// 用 tar.Reader 验证 tar 内容
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	if hdr.Name != "test.txt" {
		t.Errorf("expected test.txt, got %q", hdr.Name)
	}
	content, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read tar content: %v", err)
	}
	if string(content) != "data" {
		t.Errorf("expected data, got %q", string(content))
	}
	// 确保没有更多 entry
	if _, err := tr.Next(); err != io.EOF {
		t.Error("expected EOF after single entry")
	}
}

func TestClientArchiveDir(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ArchiveDir 发送 GET /api/archive-dir?dirname=xxx
		if r.Method != "GET" || r.URL.Path != "/api/archive-dir" {
			errCh <- fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		tw := tar.NewWriter(w)
		tw.WriteHeader(&tar.Header{
			Name:     "mydir/",
			Typeflag: tar.TypeDir,
		})
		tw.WriteHeader(&tar.Header{
			Name: "mydir/file.txt",
			Size: 5,
		})
		tw.Write([]byte("hello"))
		tw.Close()
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	dst := filepath.Join(t.TempDir(), "dir.tar")

	err := c.ArchiveDir(t.Context(), "mydir", dst)
	if err != nil {
		t.Fatalf("ArchiveDir() = %v", err)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("archive dir file is empty")
	}

	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestClientArchive_ServerError(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"Success": false, "Message": "internal error"})
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.Archive(t.Context(), []string{"x.txt"}, filepath.Join(t.TempDir(), "out.tar"))
	if err == nil {
		t.Error("expected error for server 500, got nil")
	}
}

// TestClientArchive_HTTPError 测试 downloadToFile 中的 HTTP 错误路径。
func TestClientArchive_HTTPError(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad request`))
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.Archive(t.Context(), []string{"x.txt"}, filepath.Join(t.TempDir(), "out.tar"))
	if err == nil {
		t.Error("expected error for HTTP 400, got nil")
	}
}

// TestClientArchive_WriteError 测试 downloadToFile 中写入失败（只读目录）时的清理。
func TestClientArchive_WriteError(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 返回大量数据，但尝试写入只读目录
		_, _ = w.Write([]byte("some archive data"))
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	// 用不存在的目录路径，os.Create 会失败
	err := c.Archive(t.Context(), []string{"x.txt"}, filepath.Join(t.TempDir(), "nonexistent", "out.tar"))
	if err == nil {
		t.Error("expected error for write failure, got nil")
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}

// TestClientArchive_DirHTTPError 测试 ArchiveDir 中 HTTP 错误路径。
func TestClientArchive_DirHTTPError(t *testing.T) {
	t.Parallel()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`not found`))
	}))
	defer mock.Close()

	c := NewFileClient(mock.URL, WithTimeout(5*time.Second))
	err := c.ArchiveDir(t.Context(), "nonexistent", filepath.Join(t.TempDir(), "out.tar"))
	if err == nil {
		t.Error("expected error for HTTP 404, got nil")
	}
}
