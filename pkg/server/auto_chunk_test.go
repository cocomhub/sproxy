// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// auto_chunk_test.go 验证大文件自动转分块回退（roadmap 2.3 P0）：
//  1. 普通上传超过 max_upload_bytes → 413 + X-Auto-Chunked: true 头（客户端据此转分块）。
//  2. 未超限 → 200 无头（零回归）。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

// TestUpload_OverLimitReturnsAutoChunkHeader 超限普通上传 → 413 + X-Auto-Chunked 头。
func TestUpload_OverLimitReturnsAutoChunkHeader(t *testing.T) {
	t.Parallel()
	// max_upload_bytes 调小（16 KiB）→ 构造 64 KiB 合法 multipart 上传。
	url, _, cleanup := newTestServer(t, func(c *Config) {
		c.MaxUploadBytes = 16 * 1024
	})
	defer cleanup()

	payload := bytes.Repeat([]byte("x"), 64*1024)
	sum := sha256.Sum256(payload)
	cs := hex.EncodeToString(sum[:])

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, ferr := mw.CreateFormFile("file", "big.bin")
	if ferr != nil {
		t.Fatalf("CreateFormFile: %v", ferr)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req, err := http.NewRequest("POST", url+"/upload", &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", cs)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if v := resp.Header.Get("X-Auto-Chunked"); v != "true" {
		t.Fatalf("X-Auto-Chunked = %q, want true（客户端据此转分块）", v)
	}
}

// TestUpload_WithinLimitNoHeader 未超限 → 200 无自动分块头（零回归）。
func TestUpload_WithinLimitNoHeader(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, func(c *Config) {
		c.MaxUploadBytes = 16 * 1024
	})
	defer cleanup()

	payload := []byte("small file")
	sum := sha256.Sum256(payload)
	cs := hex.EncodeToString(sum[:])

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, ferr := mw.CreateFormFile("file", "small.txt")
	if ferr != nil {
		t.Fatalf("CreateFormFile: %v", ferr)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req, err := http.NewRequest("POST", url+"/upload", &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", cs)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if v := resp.Header.Get("X-Auto-Chunked"); v != "" {
		t.Fatalf("X-Auto-Chunked 不应出现（未超限）, got %q", v)
	}
}

var _ = fmt.Sprintf
