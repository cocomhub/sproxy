// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
)

func TestUploadStore_CreateAndGet(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-test-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, err := us.CreateSession("test-upload-id", "test.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if session.UploadID == "" {
		t.Fatal("upload_id should not be empty")
	}

	got := us.GetSession(session.UploadID)
	if got == nil {
		t.Fatal("GetSession returned nil")
		return
	}
	if got.Filename != "test.txt" {
		t.Fatalf("filename mismatch: %s", got.Filename)
	}
}

func TestUploadStore_MarkAndCheck(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-mark-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-2", "test.txt", 8192, 4096, 2, strings.Repeat("b", 64), 0)

	if us.AllChunksReceived(session.UploadID) {
		t.Fatal("should not have all chunks before any upload")
	}

	us.MarkChunkReceived(session.UploadID, 0, "chunk0hash")
	if us.AllChunksReceived(session.UploadID) {
		t.Fatal("should not have all chunks after only first")
	}

	us.MarkChunkReceived(session.UploadID, 1, "chunk1hash")
	if !us.AllChunksReceived(session.UploadID) {
		t.Fatal("should have all chunks after both")
	}
}

func TestUploadStore_Complete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-complete-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-3", "test.txt", 100, 4096, 1, strings.Repeat("c", 64), 0)
	us.MarkChunkReceived(session.UploadID, 0, "chunkhash")
	us.CompleteSession(session.UploadID)

	got := us.GetSession(session.UploadID)
	if !got.Completed {
		t.Fatal("session should be completed")
	}

	// 重复 complete 应返回错误
	err = us.CompleteSession(session.UploadID)
	if err == nil {
		t.Fatal("expected error on double complete")
	}
}

func TestUploadStore_MissingChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-missing-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-4", "test.txt", 8192, 4096, 2, strings.Repeat("d", 64), 0)

	us.MarkChunkReceived(session.UploadID, 0, "h0")

	missing := MissingChunks(us.GetSession(session.UploadID))
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("expected missing [1], got %v", missing)
	}
}

func TestNegotiateChunkSize_EdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		clientSize int64
		cfgSize    int64
		want       int64
		wantAdj    bool
	}{
		{
			name:       "client 64 MiB exact",
			clientSize: 64 * 1024 * 1024,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    true,
		},
		{
			name:       "client 0, cfg 4 MiB",
			clientSize: 0,
			cfgSize:    4 * 1024 * 1024,
			want:       4 * 1024 * 1024,
			wantAdj:    false,
		},
		{
			name:       "client 0, cfg 0",
			clientSize: 0,
			cfgSize:    0,
			want:       size.DefaultChunkSize,
			wantAdj:    false,
		},
		{
			name:       "client below margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin - 1,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin - 1,
			wantAdj:    false,
		},
		{
			name:       "client at margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    false,
		},
		{
			name:       "client above margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin + 1,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, adj := negotiateChunkSize(tt.clientSize, tt.cfgSize)
			if got != tt.want {
				t.Errorf("negotiateChunkSize(%d, %d) = %d, want %d", tt.clientSize, tt.cfgSize, got, tt.want)
			}
			if adj != tt.wantAdj {
				t.Errorf("negotiateChunkSize(%d, %d) adjusted = %v, want %v", tt.clientSize, tt.cfgSize, adj, tt.wantAdj)
			}
		})
	}
}

// sha256Hex 返回 data 的 SHA-256 十六进制摘要（测试辅助）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
