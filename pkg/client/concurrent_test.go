// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestConcurrentChunkedUpload 测试并发分块上传无竞态问题。
func TestConcurrentChunkedUpload(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var mu sync.Mutex
	chunkCalls := 0
	completeCalls := 0

	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"test-concurrent"}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		chunkCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		completeCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"test-concurrent","file_checksum":"abc"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Create a test file with multiple chunks
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "concurrent.dat")
	fileData := bytes.Repeat([]byte("A"), testChunkSize*4)
	if err := os.WriteFile(filePath, fileData, 0644); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan string, 5)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := NewFileClient(ts.URL)
			remoteName := "concurrent_" + strconv.Itoa(n) + ".dat"
			result, err := c.ChunkedUpload(t.Context(), filePath, remoteName,
				WithChunkedChunkSize(testChunkSize),
				WithChunkedConcurrency(2),
				WithChunkedResume(false),
			)
			if err != nil {
				errCh <- fmt.Sprintf("ChunkedUpload #%d failed: %v", n, err)
				return
			}
			if result == nil || !result.Success {
				errCh <- fmt.Sprintf("ChunkedUpload #%d result not successful: %+v", n, result)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for msg := range errCh {
		t.Error(msg)
	}

	mu.Lock()
	if chunkCalls == 0 {
		t.Error("expected at least one chunk upload call")
	}
	if completeCalls == 0 {
		t.Error("expected at least one complete call")
	}
	mu.Unlock()
}

// TestConcurrentFileOperations 测试并发 Stat 操作无竞态问题。
// 注意：Stat 方法本身通过 HEAD 请求取文件元信息，无共享状态，
// 并发调用应安全通过 -race 检测。
func TestConcurrentFileOperations(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
		w.Header().Set("X-File-Size", "42")
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	errCh := make(chan string, 10)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := NewFileClient(ts.URL)
			info, err := c.Stat(t.Context(), "test.txt")
			if err != nil {
				errCh <- fmt.Sprintf("concurrent stat #%d failed: %v", n, err)
				return
			}
			if info.Size != 42 {
				errCh <- fmt.Sprintf("concurrent stat #%d: expected size 42, got %d", n, info.Size)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for msg := range errCh {
		t.Error(msg)
	}
}
