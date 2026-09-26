// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// archive_compression_test.go 验证归档压缩算法选型（roadmap 11.10-⑨ 压缩算法扩展）：
//  1. compression 空 → 默认 gzip（零回归，Content-Type application/gzip + .tar.gz）。
//  2. compression=zstd → zstd 归档（Content-Type application/zstd + 内容解压一致）。
//  3. compression=brotli → brotli 归档（Content-Type application/x-brotli + 内容解压一致）。
//  4. compression=unknown → 400（不静默回退 gzip）。

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/compressx"
)

// archiveCompressionRoundTrip 用给定压缩算法创建归档并验证内容一致。
// 返回响应 Content-Type 与 Content-Disposition（供断言可观测性）。
func archiveCompressionRoundTrip(t *testing.T, baseURL, comp string) (string, string) {
	t.Helper()
	body := []byte("hello compression world")
	if st := uploadFileSigned(t, baseURL, "c.txt", body); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	payload := map[string]any{"files": []string{"c.txt"}}
	if comp != "" {
		payload["compression"] = comp
	}
	reqBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/archive", bytes.NewReader(reqBody))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, reqBody)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive compression=%s = %d", comp, resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	cd := resp.Header.Get("Content-Disposition")

	algo, perr := compressx.Parse(comp)
	if comp == "" {
		algo, perr = compressx.Gzip, nil
	}
	if perr != nil {
		t.Fatalf("Parse(%q): %v", comp, perr)
	}
	zr, rerr := compressx.NewReader(algo, resp.Body)
	if rerr != nil {
		t.Fatalf("NewReader(%s): %v", comp, rerr)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	hdr, herr := tr.Next()
	if herr != nil {
		t.Fatalf("tar next: %v", herr)
	}
	if hdr.Name != "c.txt" {
		t.Fatalf("tar entry = %q, want c.txt", hdr.Name)
	}
	content, _ := io.ReadAll(tr)
	if string(content) != "hello compression world" {
		t.Fatalf("解压内容 = %q, want 原文", content)
	}
	return ct, cd
}

// TestArchiveCompression_DefaultGzip 空 compression → 默认 gzip（零回归）。
func TestArchiveCompression_DefaultGzip(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	ct, cd := archiveCompressionRoundTrip(t, baseURL, "")
	if ct != "application/gzip" {
		t.Fatalf("Content-Type = %q, want application/gzip", ct)
	}
	if !bytes.Contains([]byte(cd), []byte(".tar.gz")) {
		t.Fatalf("Content-Disposition = %q, want .tar.gz", cd)
	}
}

// TestArchiveCompression_Zstd zstd 归档 round-trip。
func TestArchiveCompression_Zstd(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	ct, cd := archiveCompressionRoundTrip(t, baseURL, "zstd")
	if ct != "application/zstd" {
		t.Fatalf("Content-Type = %q, want application/zstd", ct)
	}
	if !bytes.Contains([]byte(cd), []byte(".tar.zst")) {
		t.Fatalf("Content-Disposition = %q, want .tar.zst", cd)
	}
}

// TestArchiveCompression_Brotli brotli 归档 round-trip。
func TestArchiveCompression_Brotli(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	ct, cd := archiveCompressionRoundTrip(t, baseURL, "brotli")
	if ct != "application/x-brotli" {
		t.Fatalf("Content-Type = %q, want application/x-brotli", ct)
	}
	if !bytes.Contains([]byte(cd), []byte(".tar.br")) {
		t.Fatalf("Content-Disposition = %q, want .tar.br", cd)
	}
}

// TestArchiveCompression_UnknownAlgo 非法算法 → 400（不静默回退 gzip）。
func TestArchiveCompression_UnknownAlgo(t *testing.T) {
	t.Parallel()
	baseURL, _, cleanup := newTestServerCreds(t, nil)
	defer cleanup()
	body := []byte("x")
	if st := uploadFileSigned(t, baseURL, "d.txt", body); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	reqBody := []byte(`{"files":["d.txt"],"compression":"lzma-not-registered"}`)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/archive", bytes.NewReader(reqBody))
	signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, reqBody)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知压缩算法应 400, got %d", resp.StatusCode)
	}
	var sr struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Success {
		t.Fatal("未知压缩算法应 success=false")
	}
}
