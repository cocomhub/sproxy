// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/server"
)

// makeKey 生成一个合法的 64 位 hex AES-256 密钥（重复 'a'）。
func makeKey() string { return strings.Repeat("a", 64) }

// startTestServer 启动一个完整 sproxy 测试服务（含 tunnel 路由），返回 URL 和清理函数。
func startTestServer(t *testing.T) (string, string) {
	t.Helper()
	tmpDir := t.TempDir()

	cfg := server.Default()
	cfg.UploadsDir = tmpDir
	cfg.TunnelKey = makeKey()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var cfgPtr atomic.Pointer[server.Config]
	cfgPtr.Store(cfg)

	key, _ := hex.DecodeString(cfg.TunnelKey)
	mux := http.NewServeMux()
	h := server.RegisterRoutes(context.Background(), mux, &cfgPtr, "v", "t", key, nil, nil)
	t.Cleanup(func() { _ = h.Close() })

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, tmpDir
}

// TestE2E_Direct_UploadStatRenameDownloadDelete 不走隧道，直接调用 sproxy。
// 端到端：upload → stat → rename → download → delete，每步验证 SHA-256 一致。
func TestE2E_Direct_UploadStatRenameDownloadDelete(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	// 准备本地文件
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "data.bin")
	payload := []byte("hello sproxy e2e — 中文测试 — 0123456789")
	if err := os.WriteFile(srcPath, payload, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	wantCS := hex.EncodeToString(sum[:])

	c := client.NewFileClient(url)

	// 1. upload
	if _, err := c.Upload(context.Background(), srcPath, "data.bin"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// 2. stat
	info, err := c.Stat(context.Background(), "data.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Checksum != wantCS {
		t.Fatalf("stat checksum mismatch: want %s, got %s", wantCS, info.Checksum)
	}

	// 3. rename
	if err := c.Rename(context.Background(), "data.bin", "sub/dir/renamed.bin", info.Checksum); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// 4. download
	outPath := filepath.Join(t.TempDir(), "out.bin")
	if err := c.Download(context.Background(), "sub/dir/renamed.bin", outPath); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(got)
	if hex.EncodeToString(gotSum[:]) != wantCS {
		t.Fatalf("downloaded content checksum mismatch")
	}

	// 5. delete（不传 localPath，依赖远端 stat 获取 checksum）
	if err := c.Delete(context.Background(), "sub/dir/renamed.bin", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestE2E_Tunnel_UploadDownload 通过 AES-256-GCM 隧道完成 upload + download，
// 验证整条加密链路。
func TestE2E_Tunnel_UploadDownload(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "tunnel.txt")
	payload := []byte("tunnel data — encrypted end-to-end")
	if err := os.WriteFile(srcPath, payload, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	wantCS := hex.EncodeToString(sum[:])

	c := client.NewFileClient(url, client.WithTunnel(makeKey()))

	if _, err := c.Upload(context.Background(), srcPath, "tunnel.txt"); err != nil {
		t.Fatalf("Upload via tunnel: %v", err)
	}

	info, err := c.Stat(context.Background(), "tunnel.txt")
	if err != nil {
		t.Fatalf("Stat via tunnel: %v", err)
	}
	if info.Checksum != wantCS {
		t.Fatalf("checksum mismatch: want %s, got %s", wantCS, info.Checksum)
	}

	outPath := filepath.Join(t.TempDir(), "tunnel.out")
	if err := c.Download(context.Background(), "tunnel.txt", outPath); err != nil {
		t.Fatalf("Download via tunnel: %v", err)
	}
	got, _ := os.ReadFile(outPath)
	gotSum := sha256.Sum256(got)
	if hex.EncodeToString(gotSum[:]) != wantCS {
		t.Fatalf("tunneled content checksum mismatch")
	}
}

// TestE2E_RangeDownload 验证 /download 支持标准 Range header（PR-5）。
func TestE2E_RangeDownload(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	// 直接上传一个 1024 字节的文件
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "ranged.bin")
	payload := make([]byte, 1024)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	if err := os.WriteFile(srcPath, payload, 0644); err != nil {
		t.Fatal(err)
	}
	c := client.NewFileClient(url)
	if _, err := c.Upload(context.Background(), srcPath, "ranged.bin"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// 自己发 Range 请求
	req, _ := http.NewRequest("GET", url+"/download?filename=ranged.bin", nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("want 206, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got == "" {
		t.Fatal("missing Content-Range header")
	}
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	if n != 10 {
		t.Fatalf("expected 10 bytes for bytes=10-19, got %d", n)
	}
	for i := range 10 {
		if buf[i] != byte((10+i)&0xff) {
			t.Fatalf("byte %d mismatch: want %d, got %d", i, byte(10+i), buf[i])
		}
	}
}

// ---------- 并发测试 ----------

func TestConcurrent_UploadDifferentFiles(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	var wg sync.WaitGroup
	errCh := make(chan error, 20)

	for i := range 20 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			srcDir := t.TempDir()
			data := fmt.Appendf(nil, "concurrent file %d content", n)
			srcPath := filepath.Join(srcDir, fmt.Sprintf("f%d.txt", n))
			if err := os.WriteFile(srcPath, data, 0644); err != nil {
				errCh <- err
				return
			}
			c := client.NewFileClient(url)
			if _, err := c.Upload(context.Background(), srcPath, fmt.Sprintf("f%d.txt", n)); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent upload error: %v", err)
		}
	}
}

func TestConcurrent_UploadSameFile(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	srcDir := t.TempDir()
	data := []byte("same content for all uploads")
	srcPath := filepath.Join(srcDir, "same.txt")
	if err := os.WriteFile(srcPath, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var wg sync.WaitGroup
	successCount := int32(0)
	for range 10 {
		wg.Go(func() {
			c := client.NewFileClient(url)
			if result, err := c.Upload(context.Background(), srcPath, "same.txt"); err == nil && result.Success {
				atomic.AddInt32(&successCount, 1)
			}
		})
	}
	wg.Wait()

	if successCount < 1 {
		t.Fatal("at least one upload should succeed")
	}
	c := client.NewFileClient(url)
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "same.txt")
	if err := c.Download(context.Background(), "same.txt", outPath); err != nil {
		t.Fatalf("download after concurrent: %v", err)
	}
	got, _ := os.ReadFile(outPath)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded content mismatch after concurrent upload")
	}
}

func TestConcurrent_RenameAndDelete(t *testing.T) {
	t.Parallel()
	url, _ := startTestServer(t)

	srcDir := t.TempDir()
	data := []byte("concurrent rename/delete target")
	srcPath := filepath.Join(srcDir, "target.txt")
	if err := os.WriteFile(srcPath, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := client.NewFileClient(url)
	if _, err := c.Upload(context.Background(), srcPath, "target.txt"); err != nil {
		t.Fatalf("upload: %v", err)
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			info, err := c.Stat(context.Background(), "target.txt")
			if err != nil {
				return
			}
			_ = c.Rename(context.Background(), "target.txt", "moved.txt", info.Checksum)
		})
	}
	for range 5 {
		wg.Go(func() {
			_ = c.Delete(context.Background(), "target.txt", "")
		})
	}
	wg.Wait()
}

// ---------- 辅助函数 ----------

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---------- 混沌测试（崩溃恢复） ----------

func TestChaos_CrashDuringChunkedUpload(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// 阶段1: 创建 session 并上传部分分块
	us1 := server.NewUploadStore(tmpDir, 24*time.Hour, nil)

	fileData := bytes.Repeat([]byte("ChaosTest"), 2048)
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)
	totalChunks := 4

	us1.CreateSession("crash-test-id", "crash-recover.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum, 0)
	for i := range 2 {
		chunkData := fileData[i*int(chunkSize) : (i+1)*int(chunkSize)]
		chunkCS := sha256hex(chunkData)

		chunkDir := filepath.Join(tmpDir, ".__chunked__", "crash-test-id")
		os.MkdirAll(chunkDir, 0755)
		os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), chunkData, 0644)

		us1.MarkChunkReceived("crash-test-id", i, chunkCS)
	}
	us1.Stop() // 模拟 crash

	// 阶段2: 新实例 recover
	us2 := server.NewUploadStore(tmpDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("crash-test-id")
	if s == nil {
		t.Fatal("session should be recovered after crash")
	}
	if !s.ReceivedChunks[0] || !s.ReceivedChunks[1] {
		t.Fatal("chunks 0 and 1 should be recovered")
	}
	if s.ReceivedChunks[2] || s.ReceivedChunks[3] {
		t.Fatal("chunks 2 and 3 should not be marked")
	}

	// 上传剩余分块
	for i := 2; i < totalChunks; i++ {
		start := i * int(chunkSize)
		end := start + int(chunkSize)
		chunkData := fileData[start:end]
		chunkCS := sha256hex(chunkData)
		chunkDir := filepath.Join(tmpDir, ".__chunked__", "crash-test-id")
		os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), chunkData, 0644)
		us2.MarkChunkReceived("crash-test-id", i, chunkCS)
	}

	if !us2.AllChunksReceived("crash-test-id") {
		t.Fatal("all chunks should be received after resume")
	}
}

func TestChaos_PartialChunkWrittenThenRecover(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	us1 := server.NewUploadStore(tmpDir, 24*time.Hour, nil)
	us1.CreateSession("partial-id", "partial-recover.bin", 8192, 4096, 2, strings.Repeat("x", 64), 0)
	us1.Stop()

	// 手动写入 chunk 文件但不调用 MarkChunkReceived (模拟 crash)
	chunkDir := filepath.Join(tmpDir, ".__chunked__", "partial-id")
	os.MkdirAll(chunkDir, 0755)

	for i := range 2 {
		data := bytes.Repeat([]byte{byte(i)}, 4096)
		os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), data, 0644)
	}

	us2 := server.NewUploadStore(tmpDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("partial-id")
	if s == nil {
		t.Fatal("session should be recovered")
	}
	t.Logf("recovered session: received=%v", s.ReceivedChunks)
}

func TestChaos_ChecksumStoreCrashAtomic(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	cs := server.NewChecksumStore(tmpDir, nil)
	cs.Set("k1", "v1")
	cs.Set("k2", "v2")

	// 模拟 crash: 创建一个 .tmp 残留文件
	tmpFile := filepath.Join(tmpDir, ".checksums.json.tmp")
	os.WriteFile(tmpFile, []byte(`{"stale":"data"}`), 0644)

	// 新实例: 应清理 .tmp 并正确加载已持久化的 .json
	cs2 := server.NewChecksumStore(tmpDir, nil)
	all := cs2.GetAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d; tmp residue was not cleaned", len(all))
	}
	// 确认 .tmp 已清理
	if _, err := os.Stat(tmpFile); err == nil {
		t.Fatal(".tmp should be cleaned up on startup")
	}
}
