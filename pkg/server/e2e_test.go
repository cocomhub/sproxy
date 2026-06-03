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
	h := server.RegisterRoutes(context.Background(), mux, &cfgPtr, "v", "t", key, nil)
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
			data := []byte(fmt.Sprintf("concurrent file %d content", n))
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := client.NewFileClient(url)
			if result, err := c.Upload(context.Background(), srcPath, "same.txt"); err == nil && result.Success {
				atomic.AddInt32(&successCount, 1)
			}
		}()
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := c.Stat(context.Background(), "target.txt")
			if err != nil {
				return
			}
			_ = c.Rename(context.Background(), "target.txt", "moved.txt", info.Checksum)
		}()
	}
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Delete(context.Background(), "target.txt", "")
		}()
	}
	wg.Wait()
}
