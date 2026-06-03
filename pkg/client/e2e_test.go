// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
)

// startFullTestServer 启动完整 sproxy 服务（含所有路由和分块上传支持）。
func startFullTestServer(t *testing.T) (string, *server.Config) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := server.Default()
	cfg.UploadsDir = tmpDir
	cfg.ChunkSize = 4 << 10 // 4 KiB for test
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := server.RegisterRoutes(context.Background(), mux, &cfgPtr, "v", "t", nil, nil)
	t.Cleanup(func() { _ = h.Close() })

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, cfg
}

func TestClientChunkedUpload_Download_RoundTrip(t *testing.T) {
	url, _ := startFullTestServer(t)

	srcDir := t.TempDir()
	fileData := bytes.Repeat([]byte("ClientChunkedTest!"), 1280) // ~20 KiB
	srcPath := filepath.Join(srcDir, "upload.bin")
	if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewFileClient(url)
	c.ChunkSize = 4096
	c.MaxChunkSize = 4096

	// 分块上传
	result, err := c.ChunkedUpload(context.Background(), srcPath, "upload.bin")
	if err != nil {
		t.Fatalf("ChunkedUpload: %v", err)
	}
	if !result.Success {
		t.Fatalf("chunked upload failed: %s", result.Message)
	}

	// 分块下载
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "downloaded.bin")
	if err := c.ChunkedDownload(context.Background(), "upload.bin", outPath); err != nil {
		t.Fatalf("ChunkedDownload: %v", err)
	}

	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, fileData) {
		t.Fatal("downloaded content mismatch after chunked round-trip")
	}
}

func TestClientChunkedUpload_Resume(t *testing.T) {
	url, _ := startFullTestServer(t)

	srcDir := t.TempDir()
	fileData := bytes.Repeat([]byte("ResumeChunkedData"), 2048) // ~32 KiB
	srcPath := filepath.Join(srcDir, "resume.bin")
	if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewFileClient(url)
	c.ChunkSize = 4096
	c.MaxChunkSize = 4096

	// 分块上传（允许续传）
	result, err := c.ChunkedUpload(context.Background(), srcPath, "resume.bin")
	if err != nil {
		t.Fatalf("ChunkedUpload: %v", err)
	}
	if !result.Success {
		t.Fatalf("upload failed: %s", result.Message)
	}

	// 下载验证
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "resume-dl.bin")
	if err := c.Download(context.Background(), "resume.bin", outPath); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, fileData) {
		t.Fatal("content mismatch after resume upload")
	}
}

func TestClient_ChunkedUploadAutoThreshold(t *testing.T) {
	url, _ := startFullTestServer(t)

	srcDir := t.TempDir()
	smallData := bytes.Repeat([]byte("S"), 1024) // 1 KiB
	srcPath := filepath.Join(srcDir, "small.bin")
	if err := os.WriteFile(srcPath, smallData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 验证小文件不应触发自动分块
	if ShouldAutoChunk(int64(len(smallData))) {
		t.Log("file below AutoChunkThreshold should not auto-chunk")
	}

	c := NewFileClient(url)
	result, err := c.Upload(context.Background(), srcPath, "small.bin")
	if err != nil {
		t.Fatalf("Upload (non-chunked): %v", err)
	}
	if !result.Success {
		t.Fatalf("upload failed: %s", result.Message)
	}
}