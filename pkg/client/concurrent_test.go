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

// concurrentChunkedUploadCounterHandler 返回分块上传 mock handler：
// 并发安全地自增计数并回写固定 JSON 响应体。
func concurrentChunkedUploadCounterHandler(mu *sync.Mutex, count *int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		*count++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// concurrentChunkedUploadOnce 执行一次分块上传，失败/非成功时向 errCh 推送信息。
func concurrentChunkedUploadOnce(t *testing.T, baseURL, filePath string, n int, errCh chan<- string) {
	t.Helper()
	c := NewFileClient(baseURL)
	remoteName := "concurrent_" + strconv.Itoa(n) + ".dat"
	result, err := c.ChunkedUpload(t.Context(), filePath, remoteName,
		WithChunkedChunkSize(testChunkSize),
		WithChunkedConcurrency(2),
		WithChunkedResume(false),
	)
	if err != nil {
		select {
		case errCh <- fmt.Sprintf("ChunkedUpload #%d failed: %v", n, err):
		default:
			t.Error("error channel full, dropping message")
		}
		return
	}
	if result == nil || !result.Success {
		select {
		case errCh <- fmt.Sprintf("ChunkedUpload #%d result not successful: %+v", n, result):
		default:
			t.Error("error channel full, dropping message")
		}
	}
}

// runConcurrentChunkedUploads 并发发起 5 个分块上传，收集每条错误/结果信息。
func runConcurrentChunkedUploads(t *testing.T, baseURL, filePath string) []string {
	t.Helper()
	errCh := make(chan string, 5)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			concurrentChunkedUploadOnce(t, baseURL, filePath, n, errCh)
		}(i)
	}
	wg.Wait()
	close(errCh)
	var msgs []string
	for msg := range errCh {
		msgs = append(msgs, msg)
	}
	return msgs
}

// assertConcurrentChunkedCallCounts 断言 5 个并发上传的 init/chunk/complete 调用次数。
func assertConcurrentChunkedCallCounts(t *testing.T, initCalls, chunkCalls, completeCalls int) {
	t.Helper()
	// 5 个并发上传，每个 4 个 chunk，关闭 resume
	// 预期每个上传：init(1) + chunk(4) + complete(1) = 6 次调用
	// 总 initCalls = 5 * 1 = 5
	// 总 chunkCalls = 5 * 4 = 20
	// 总 completeCalls = 5 * 1 = 5
	expectedInitCalls := 5 * 1
	expectedChunkCalls := 5 * 4
	expectedCompleteCalls := 5 * 1

	if initCalls != expectedInitCalls {
		t.Errorf("expected %d init calls, got %d", expectedInitCalls, initCalls)
	}
	if chunkCalls != expectedChunkCalls {
		t.Errorf("expected %d chunk upload calls, got %d", expectedChunkCalls, chunkCalls)
	}
	if completeCalls != expectedCompleteCalls {
		t.Errorf("expected %d complete calls, got %d", expectedCompleteCalls, completeCalls)
	}
}

// TestConcurrentChunkedUpload 测试并发分块上传无竞态问题。
func TestConcurrentChunkedUpload(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var mu sync.Mutex
	chunkCalls := 0
	completeCalls := 0
	initCalls := 0

	mux.HandleFunc("POST /upload/init", concurrentChunkedUploadCounterHandler(&mu, &initCalls, `{"success":true,"upload_id":"test-concurrent"}`))
	mux.HandleFunc("POST /upload/chunk", concurrentChunkedUploadCounterHandler(&mu, &chunkCalls, `{"success":true}`))
	mux.HandleFunc("POST /upload/complete", concurrentChunkedUploadCounterHandler(&mu, &completeCalls, `{"success":true,"upload_id":"test-concurrent","file_checksum":"abc"}`))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Create a test file with multiple chunks
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "concurrent.dat")
	fileData := bytes.Repeat([]byte("A"), testChunkSize*4)
	if err := os.WriteFile(filePath, fileData, 0644); err != nil {
		t.Fatal(err)
	}

	for _, msg := range runConcurrentChunkedUploads(t, ts.URL, filePath) {
		t.Error(msg)
	}

	mu.Lock()
	assertConcurrentChunkedCallCounts(t, initCalls, chunkCalls, completeCalls)
	mu.Unlock()
}

// TestConcurrentFileOperations 测试并发 Stat 操作无竞态问题。
// 注意：Stat 方法本身通过 HEAD 请求取文件元信息，无共享状态，
// 并发调用应安全通过 -race 检测。
func TestConcurrentFileOperations(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		// 校验 filename 参数
		if r.URL.Query().Get("filename") == "" {
			http.Error(w, "missing filename", http.StatusBadRequest)
			return
		}
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
