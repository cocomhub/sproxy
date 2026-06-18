// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
