// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientArchive_SingleFile(t *testing.T) {
	t.Parallel()

	errCh := make(chan error, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/archive" {
			errCh <- fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			return
		}
		// 检查请求体包含 {"files":["test.txt"]}
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

	close(errCh)
	for err := range errCh {
		t.Error(err)
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
