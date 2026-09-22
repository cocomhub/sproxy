// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// auto_chunk_test.go 验证大文件自动转分块回退（roadmap 2.3 P0）：
//  1. /upload 返回 413 + X-Auto-Chunked: true → Upload 自动重试 ChunkedUpload（成功）。
//  2. 413 无头 → 保持错误（不转分块，防误判）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestUpload_AutoChunkFallback 413+头 → 自动转分块成功。
func TestUpload_AutoChunkFallback(t *testing.T) {
	t.Parallel()
	var initN, completeN atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerAutoChunk, "true")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"success":false,"message":"too large"}`))
	})
	mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"message":"not found"}`))
	})
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			UploadID string `json:"upload_id"`
		}
		_ = json.Unmarshal(body, &req)
		initN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"` + req.UploadID + `","chunk_size":1024}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, _ *http.Request) {
		completeN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"x","file_checksum":"abc"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "big.dat")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	c := NewFileClient(ts.URL, WithChunkSize(1024))
	res, err := c.Upload(t.Context(), filePath, "big.dat")
	if err != nil {
		t.Fatalf("Upload (auto chunk fallback): %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if initN.Load() != 1 {
		t.Fatalf("init calls = %d, want 1（自动转分块）", initN.Load())
	}
	if completeN.Load() != 1 {
		t.Fatalf("complete calls = %d, want 1", completeN.Load())
	}
}

// TestUpload_413NoHeaderNoFallback 413 无头 → 保持错误（不转分块）。
func TestUpload_413NoHeaderNoFallback(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"success":false,"message":"too large"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "big.dat")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	c := NewFileClient(ts.URL)
	_, err := c.Upload(t.Context(), filePath, "big.dat")
	if err == nil {
		t.Fatal("413 无头应保持错误（不转分块）")
	}
}
