// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cloudArchiveDirName 测试本地镜像服务端归档存储子目录（服务端常量在 pkg/server，
// client 包不可 import，此处测试本地定义与服务端布局保持一致）。
const cloudArchiveDirName = ".__cloud_archives__"

// TestChunkedDownload_WithKindCloudArchive 验证 ChunkedDownload + WithChunkedKind(cloud_archive)
// 的 stat 与 chunk 请求均带 kind=cloud_archive，且归档内容正确落地。
func TestChunkedDownload_WithKindCloudArchive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, cloudArchiveDirName)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive dir: %v", err)
	}
	content := bytes.Repeat([]byte("abc"), 1000) // 3000 bytes，单块分片下载
	if err := os.WriteFile(filepath.Join(archiveDir, "x.tar.gz"), content, 0644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	sum := sha256.Sum256(content)

	mux := http.NewServeMux()
	var statQueries, chunkQueries []string
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		statQueries = append(statQueries, r.URL.RawQuery)
		if r.URL.Query().Get("kind") != DownloadKindCloudArchive {
			http.Error(w, "missing kind", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", len(content)))
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", time.Now().UnixNano()))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		chunkQueries = append(chunkQueries, r.URL.RawQuery)
		if r.URL.Query().Get("kind") != DownloadKindCloudArchive {
			http.Error(w, "missing kind", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(archiveDir, filepath.Base(r.URL.Query().Get("filename"))))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		length, _ := strconv.ParseInt(r.URL.Query().Get("length"), 10, 64)
		end := min(offset+length, int64(len(data)))
		w.Write(data[offset:end])
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	outPath := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := c.ChunkedDownload(t.Context(), "x.tar.gz", outPath, WithChunkedKind(DownloadKindCloudArchive)); err != nil {
		t.Fatalf("ChunkedDownload with kind: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}
	for _, q := range statQueries {
		if !strings.Contains(q, "kind=cloud_archive") {
			t.Errorf("stat query missing kind: %q", q)
		}
	}
	for _, q := range chunkQueries {
		if !strings.Contains(q, "kind=cloud_archive") {
			t.Errorf("chunk query missing kind: %q", q)
		}
	}
}

// TestDownloadCloudArchive 验证 DownloadCloudArchive 的 /download 请求带 kind=cloud_archive
// 且归档内容正确落地。
func TestDownloadCloudArchive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, cloudArchiveDirName)
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive dir: %v", err)
	}
	content := []byte("archive-content")
	if err := os.WriteFile(filepath.Join(archiveDir, "x.tar.gz"), content, 0644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	sum := sha256.Sum256(content)

	mux := http.NewServeMux()
	var dlQuery string
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		dlQuery = r.URL.RawQuery
		if r.URL.Query().Get("kind") != DownloadKindCloudArchive {
			http.Error(w, "missing kind", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(archiveDir, filepath.Base(r.URL.Query().Get("filename"))))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set(headerFileChecksum, hex.EncodeToString(sum[:]))
		w.Write(data)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL)
	outPath := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := c.DownloadCloudArchive(t.Context(), "x.tar.gz", outPath); err != nil {
		t.Fatalf("DownloadCloudArchive: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(content))
	}
	if !strings.Contains(dlQuery, "kind=cloud_archive") || !strings.Contains(dlQuery, "filename=x.tar.gz") {
		t.Errorf("download query = %q, want kind=cloud_archive & filename=x.tar.gz", dlQuery)
	}
}
