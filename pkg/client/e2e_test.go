// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
)

// startFullTestServer 启动完整 sproxy 服务（含所有路由和分块上传支持）。
// 凭据 store 化（task3）后：无 ring/store 时服务器会 bootstrap 生成 anonymous 凭据，
// authMiddleware 随之进入 SproxySig 验签路径——未配置 AK/SK 的裸请求会被 401 拒绝。
// 因此这里用 BootstrapServerCredentials 生成/载入真实凭据并注入 opts，同时把该
// AK/SK 通过返回值传给调用方（I2：以返回值注入替代包级全局，消 data race 隐患），
// 由调用方 WithAccessKey(ak, skHex) 传入 FileClient，使 e2e 走真实签名路径。
func startFullTestServer(t *testing.T) (url string, cfg *server.Config, ak, skHex string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg = server.Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10 // 4 KiB for test
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// 生成/载入 anonymous 凭据（首启生成并持久化到 <root>/anonymous/meta/）。
	// 注意：此处重要性——BootstrapServerCredentials 副作用是写 credentials.json，
	// 必须在 RegisterRoutes 之前调用；随后以相同 ring+store 注入 opts。
	ring, store, err := server.BootstrapServerCredentials(cfg, nil)
	if err != nil {
		t.Fatalf("BootstrapServerCredentials: %v", err)
	}
	keys := ring.Snapshot()
	if len(keys) == 0 || len(keys[0].Entries) == 0 || len(keys[0].Entries[0].SK) != 32 {
		t.Fatalf("BootstrapServerCredentials 未生成 32B anonymous 凭据（keys=%d）", len(keys))
	}
	ak = keys[0].AK
	skHex = hex.EncodeToString(keys[0].Entries[0].SK)

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := server.RegisterRoutes(t.Context(), server.RegisterRoutesOpts{
		Mux:             mux,
		CfgPtr:          &cfgPtr,
		Version:         "v",
		BuildAt:         "t",
		CredentialRing:  ring,
		CredentialStore: store,
	})
	t.Cleanup(func() { _ = h.Close() })

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return ts.URL, cfg, ak, skHex
}

func TestClientChunkedUpload_Download_RoundTrip(t *testing.T) {
	url, _, ak, skHex := startFullTestServer(t)

	srcDir := t.TempDir()
	fileData := bytes.Repeat([]byte("ClientChunkedTest!"), 1280) // ~20 KiB
	srcPath := filepath.Join(srcDir, "upload.bin")
	if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewFileClient(url, WithAccessKey(ak, skHex))
	c.chunkSize = 4096
	c.maxChunkSize = 4096

	// 分块上传
	result, err := c.ChunkedUpload(t.Context(), srcPath, "upload.bin")
	if err != nil {
		t.Fatalf("ChunkedUpload: %v", err)
	}
	if !result.Success {
		t.Fatalf("chunked upload failed: %s", result.Message)
	}

	// 分块下载
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "downloaded.bin")
	if err = c.ChunkedDownload(t.Context(), "upload.bin", outPath); err != nil {
		t.Fatalf("ChunkedDownload: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读取下载文件失败: %v", err)
	}
	if !bytes.Equal(got, fileData) {
		t.Fatal("downloaded content mismatch after chunked round-trip")
	}
}

func TestClientChunkedUpload_ThenRegularDownload(t *testing.T) {
	url, _, ak, skHex := startFullTestServer(t)

	srcDir := t.TempDir()
	fileData := bytes.Repeat([]byte("ChunkedTestData"), 2048) // ~32 KiB
	srcPath := filepath.Join(srcDir, "chunked.bin")
	if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := NewFileClient(url, WithAccessKey(ak, skHex))
	c.chunkSize = 4096
	c.maxChunkSize = 4096

	// 分块上传
	result, err := c.ChunkedUpload(t.Context(), srcPath, "chunked.bin")
	if err != nil {
		t.Fatalf("ChunkedUpload: %v", err)
	}
	if !result.Success {
		t.Fatalf("upload failed: %s", result.Message)
	}

	// 下载验证
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "dl.bin")
	if err = c.Download(t.Context(), "chunked.bin", outPath); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读取下载文件失败: %v", err)
	}
	if !bytes.Equal(got, fileData) {
		t.Fatal("content mismatch after chunked upload")
	}
}

func TestClient_SmallFileUploadWithoutChunking(t *testing.T) {
	url, _, ak, skHex := startFullTestServer(t)

	srcDir := t.TempDir()
	smallData := bytes.Repeat([]byte("S"), 1024) // 1 KiB
	srcPath := filepath.Join(srcDir, "small.bin")
	if err := os.WriteFile(srcPath, smallData, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 验证小文件不应触发自动分块
	if ShouldAutoChunk(int64(len(smallData))) {
		t.Fatal("file below AutoChunkThreshold should not auto-chunk")
	}

	c := NewFileClient(url, WithAccessKey(ak, skHex))
	result, err := c.Upload(t.Context(), srcPath, "small.bin")
	if err != nil {
		t.Fatalf("Upload (non-chunked): %v", err)
	}
	if !result.Success {
		t.Fatalf("upload failed: %s", result.Message)
	}
}
