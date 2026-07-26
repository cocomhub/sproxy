// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testChunkSize = 1024

func TestCalcChunkSize_EdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		fileSize       int64
		preferred      int64
		maxChunk       int64
		expectPositive bool
	}{
		{"zero file size", 0, 4 * 1024 * 1024, 64 * 1024 * 1024, true},
		{"preferred zero", 1024, 0, 64 * 1024 * 1024, true},
		{"maxChunk zero", 1024, 4 * 1024 * 1024, 0, true},
		{"all zero", 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := calcChunkSize(tt.fileSize, tt.preferred, tt.maxChunk)
			if tt.expectPositive && cs <= 0 {
				t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected > 0", tt.fileSize, tt.preferred, tt.maxChunk, cs)
			}
		})
	}
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestDownloadOneChunk_ExponentialBackoff(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var attempt atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Chunk-Checksum", sha256hex([]byte("test data")))
		w.Write([]byte("test data"))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "out.dat")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	if err := outFile.Truncate(9); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var progress int64
	var downloadErr error

	c.downloadOneChunk(ctx, downloadChunkParams{
		Filename:    "f.txt",
		ChunkIdx:    0,
		ChunkSize:   1024,
		FileSize:    9,
		OutFile:     outFile,
		Mu:          &mu,
		Progress:    &progress,
		DownloadErr: &downloadErr,
	})

	if downloadErr != nil {
		t.Fatalf("unexpected error: %v", downloadErr)
	}

	got := make([]byte, 9)
	if _, err := outFile.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "test data" {
		t.Errorf("expected 'test data', got %s", string(got))
	}
	if n := attempt.Load(); n != 3 {
		t.Errorf("expected 3 attempts, got %d", n)
	}
}

func TestDownloadOneChunk_RetryThenSuccess(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	var attempt atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Chunk-Checksum", sha256hex([]byte("data")))
		w.Write([]byte("data"))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "out.dat")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	if err := outFile.Truncate(4); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var progress int64
	var downloadErr error

	c.downloadOneChunk(ctx, downloadChunkParams{
		Filename:    "f.txt",
		ChunkIdx:    0,
		ChunkSize:   1024,
		FileSize:    4,
		OutFile:     outFile,
		Mu:          &mu,
		Progress:    &progress,
		DownloadErr: &downloadErr,
	})

	if downloadErr != nil {
		t.Fatalf("unexpected error: %v", downloadErr)
	}

	got := make([]byte, 4)
	if _, err := outFile.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("expected 'data', got %s", string(got))
	}
	if n := attempt.Load(); n != 3 {
		t.Errorf("expected 3 attempts, got %d", n)
	}
}

func TestDownloadOneChunk_AllRetriesFail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "out.dat")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	var mu sync.Mutex
	var progress int64
	var downloadErr error

	c.downloadOneChunk(ctx, downloadChunkParams{
		Filename:    "f.txt",
		ChunkIdx:    0,
		ChunkSize:   1024,
		FileSize:    9,
		OutFile:     outFile,
		Mu:          &mu,
		Progress:    &progress,
		DownloadErr: &downloadErr,
	})

	if downloadErr == nil {
		t.Fatal("expected error after all retries exhausted")
	}
}

func TestDownloadOneChunk_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 立即取消

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	c.logger = testLogger()

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "out.dat")
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	var mu sync.Mutex
	var progress int64
	var downloadErr error

	// 上下文已取消，downloadOneChunk 应提前返回且不设置 downloadErr
	c.downloadOneChunk(ctx, downloadChunkParams{
		Filename:    "f.txt",
		ChunkIdx:    0,
		ChunkSize:   1024,
		FileSize:    9,
		OutFile:     outFile,
		Mu:          &mu,
		Progress:    &progress,
		DownloadErr: &downloadErr,
	})

	if downloadErr != nil {
		t.Errorf("expected no downloadErr after context cancellation, got: %v", downloadErr)
	}
	if progress != 0 {
		t.Errorf("expected no progress after context cancellation, got %d", progress)
	}
}

func TestCalcChunkSize_SmallFile(t *testing.T) {
	t.Parallel()
	// Small file should not increase chunk size beyond preferred
	preferred := int64(4 * 1024 * 1022) // 4 MiB - 2 bytes
	cs := calcChunkSize(preferred*511, preferred, 64*1024*1024)
	if cs != preferred {
		t.Errorf("expected %d, got %d", preferred, cs)
	}
}

func TestCalcChunkSize_LargeFile(t *testing.T) {
	t.Parallel()
	// Very large file should hit max
	preferred := int64(4 * 1024 * 1023) // ~4 MiB
	maxChunk := int64(64 * 1024 * 1024)
	threeTB := int64(3 * 1024 * 1024 * 1024 * 1024)
	cs := calcChunkSize(threeTB, preferred, maxChunk)
	if cs > maxChunk {
		t.Errorf("expected <= %d, got %d", maxChunk, cs)
	}
}

func TestCalcChunkSize_Boundary(t *testing.T) {
	t.Parallel()
	// fileSize just under preferred*512 — should stay at preferred
	preferred := int64(4 * 1024 * 1023)
	cs := calcChunkSize(preferred*512-1, preferred, 64*1024*1024)
	if cs != preferred {
		t.Errorf("expected %d (preferred), got %d", preferred, cs)
	}
}

func TestGenerateUploadID_Deterministic(t *testing.T) {
	t.Parallel()
	now := time.Now()
	filename := "test.txt"
	size := int64(100)
	checksum := "abc123"

	id1 := generateUploadID(filename, size, now, checksum)
	id2 := generateUploadID(filename, size, now, checksum)
	if id1 != id2 {
		t.Errorf("expected same upload_id for same input, got %q vs %q", id1, id2)
	}
	if len(id1) != 32 {
		t.Errorf("expected 32 hex chars, got %d: %q", len(id1), id1)
	}
}

func TestGenerateUploadID_DifferentInputs(t *testing.T) {
	t.Parallel()
	now := time.Now()

	id1 := generateUploadID("a.txt", 100, now, "abc")
	id2 := generateUploadID("b.txt", 100, now, "abc")
	if id1 == id2 {
		t.Error("expected different upload_id for different filenames")
	}

	id3 := generateUploadID("a.txt", 200, now, "abc")
	if id1 == id3 {
		t.Error("expected different upload_id for different sizes")
	}
}

func TestTryDownloadChunk_LengthMismatch(t *testing.T) {
	t.Parallel()
	// Server returns body shorter than X-Chunk-Length (expectLength)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/download/chunk", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Chunk-Length", "100")
		w.Write([]byte("short")) // only 5 bytes
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	data, ok := c.tryDownloadChunk(t.Context(), "/api/download/chunk?filename=f.txt&offset=0&length=100", 100)
	if ok {
		t.Fatal("expected tryDownloadChunk to return false on length mismatch")
	}
	if data != nil {
		t.Fatal("expected nil data on length mismatch")
	}
}

func TestTryDownloadChunk_ChecksumMismatch(t *testing.T) {
	t.Parallel()
	// Server returns body with wrong X-Chunk-Checksum header
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/download/chunk", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Chunk-Checksum", "0000000000000000000000000000000000000000000000000000000000000000")
		w.Write([]byte("hello world"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	data, ok := c.tryDownloadChunk(t.Context(), "/api/download/chunk?filename=f.txt&offset=0&length=11", 11)
	if ok {
		t.Fatal("expected tryDownloadChunk to return false on checksum mismatch")
	}
	if data != nil {
		t.Fatal("expected nil data on checksum mismatch")
	}
}

func TestTryDownloadChunk_Non200(t *testing.T) {
	t.Parallel()
	// Server returns 500
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/download/chunk", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewFileClient(ts.URL)
	data, ok := c.tryDownloadChunk(t.Context(), "/api/download/chunk?filename=f.txt&offset=0&length=100", 100)
	if ok {
		t.Fatal("expected tryDownloadChunk to return false on 500 status")
	}
	if data != nil {
		t.Fatal("expected nil data on 500 status")
	}
}

// TestTryResumeSession 测试 tryResumeSession 的各种场景。
func TestTryResumeSession(t *testing.T) {
	t.Parallel()

	t.Run("file_already_exists", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"finished":true,"upload_id":"test-123"}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		c := NewFileClient(ts.URL)
		now := time.Now()
		params := resumeSessionParams{
			UploadID:     "test-123",
			Filename:     "test.txt",
			FileChecksum: "abc123",
			FileSize:     100,
			ChunkSize:    64,
			TotalChunks:  2,
			Concurrency:  1,
			ModTime:      now,
		}
		result, err, shouldContinue := c.tryResumeSession(t.Context(), params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldContinue {
			t.Fatal("expected shouldContinue=false for finished upload")
		}
		if result == nil || !result.Success {
			t.Fatal("expected success result for finished upload")
		}
	})

	t.Run("session_not_found", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		c := NewFileClient(ts.URL)
		params := resumeSessionParams{
			UploadID: "test-456",
			Filename: "test.txt",
		}
		result, err, shouldContinue := c.tryResumeSession(t.Context(), params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !shouldContinue {
			t.Fatal("expected shouldContinue=true for missing session")
		}
		if result != nil {
			t.Fatal("expected nil result for missing session")
		}
	})

	t.Run("server_error", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		c := NewFileClient(ts.URL)
		params := resumeSessionParams{
			UploadID: "test-789",
			Filename: "test.txt",
		}
		result, err, shouldContinue := c.tryResumeSession(t.Context(), params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !shouldContinue {
			t.Fatal("expected shouldContinue=true for server error")
		}
		if result != nil {
			t.Fatal("expected nil result for server error")
		}
	})

	t.Run("resume_with_missing_chunks", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		callCount := 0
		mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"upload_id":"test-123","missing_chunks":[0,1],"total_chunks":4}`))
		})
		mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		})
		mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"upload_id":"test-123","file_checksum":"abc123"}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		// Create a test file on disk
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "test.dat")
		fileData := bytes.Repeat([]byte("A"), testChunkSize*4)
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			t.Fatal(err)
		}

		c := NewFileClient(ts.URL)
		now := time.Now()
		params := resumeSessionParams{
			UploadID:     "test-123",
			Filename:     "test.dat",
			LocalPath:    filePath,
			FileChecksum: "abc123",
			FileSize:     int64(len(fileData)),
			ChunkSize:    testChunkSize,
			TotalChunks:  4,
			Concurrency:  1,
			ModTime:      now,
		}
		result, err, shouldContinue := c.tryResumeSession(t.Context(), params)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if shouldContinue {
			t.Fatal("expected shouldContinue=false for resume")
		}
		if result == nil || !result.Success {
			t.Fatal("expected success result after resume")
		}
		if callCount < 1 {
			t.Fatal("expected at least one chunk upload call")
		}
	})
}

// TestUploadChunkWithRetry 测试 uploadChunkWithRetry 的重试逻辑。
func TestUploadChunkWithRetry(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		// Create a test file
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "test.dat")
		fileData := bytes.Repeat([]byte("A"), testChunkSize)
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			t.Fatal(err)
		}

		c := NewFileClient(ts.URL)
		uploader := newChunkedUploader(chunkedUploaderOpts{
			client:      c,
			filePath:    filePath,
			uploadID:    "test-upload",
			chunkSize:   testChunkSize,
			fileSize:    int64(len(fileData)),
			totalChunks: 1,
			checksum:    "abc",
			filename:    "test.dat",
			concurrency: 1,
		})
		uploader.uploadChunkWithRetry(t.Context(), 0)
		if uploader.failed.Load() {
			t.Fatal("expected success, but failed flag is set")
		}
	})

	t.Run("retry_then_success", func(t *testing.T) {
		t.Parallel()
		var attempt atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
			n := attempt.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n < 3 {
				_, _ = w.Write([]byte(`{"success":false,"should_retry":true}`))
			} else {
				_, _ = w.Write([]byte(`{"success":true}`))
			}
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "test.dat")
		fileData := bytes.Repeat([]byte("B"), testChunkSize)
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			t.Fatal(err)
		}

		c := NewFileClient(ts.URL)
		uploader := newChunkedUploader(chunkedUploaderOpts{
			client:      c,
			filePath:    filePath,
			uploadID:    "test-retry",
			chunkSize:   testChunkSize,
			fileSize:    int64(len(fileData)),
			totalChunks: 1,
			checksum:    "abc",
			filename:    "test.dat",
			concurrency: 1,
		})
		uploader.uploadChunkWithRetry(t.Context(), 0)
		if uploader.failed.Load() {
			t.Fatal("expected eventual success, but failed flag is set")
		}
		if attempt.Load() != 3 {
			t.Fatalf("expected 3 attempts (2 retries), got %d", attempt.Load())
		}
	})

	t.Run("all_retries_fail", func(t *testing.T) {
		t.Parallel()
		var attempt atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
			attempt.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"should_retry":true}`))
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, "test.dat")
		fileData := bytes.Repeat([]byte("C"), testChunkSize)
		if err := os.WriteFile(filePath, fileData, 0644); err != nil {
			t.Fatal(err)
		}

		c := NewFileClient(ts.URL)
		uploader := newChunkedUploader(chunkedUploaderOpts{
			client:      c,
			filePath:    filePath,
			uploadID:    "test-fail",
			chunkSize:   testChunkSize,
			fileSize:    int64(len(fileData)),
			totalChunks: 1,
			checksum:    "abc",
			filename:    "test.dat",
			concurrency: 1,
		})
		uploader.uploadChunkWithRetry(t.Context(), 0)
		if !uploader.failed.Load() {
			t.Fatal("expected failed flag set after all retries exhausted")
		}
	})
}
