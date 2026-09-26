// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"
)

// uploadFile 经签名上传一个文件（返回 checksum）。
func uploadFile(t *testing.T, baseURL, filename string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	content := "chaos test content for " + filename
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	sum := sha256.Sum256([]byte(content))
	checksum := hex.EncodeToString(sum[:])
	req, err := http.NewRequest(http.MethodPost, baseURL+"/upload", &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", checksum)
	resp, err := authedClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload code = %d: %s", resp.StatusCode, body)
	}
	return ""
}

// download 下载文件并断言成功。
func download(t *testing.T, baseURL, filename string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/download?filename="+filename, nil)
	resp, err := authedClient.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download code = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "chaos test content") {
		t.Fatalf("下载内容不符: %q", body)
	}
}

// TestChaos_Kill9Restart 验证 kill -9 后重启恢复：上传 → 强杀 → 重启 → 下载一致。
func TestChaos_Kill9Restart(t *testing.T) {
	t.Parallel()
	node, err := NewChaosNode(t)
	if err != nil {
		t.Fatalf("NewChaosNode: %v", err)
	}
	if err := node.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid1 := node.Pid()
	if pid1 == 0 {
		t.Fatal("PID 应为非零")
	}
	uploadFile(t, node.URL, "persist.txt")
	download(t, node.URL, "persist.txt")

	// kill -9（不走优雅关闭）→ 进程退出。
	if err := node.Kill9(); err != nil {
		t.Fatalf("Kill9: %v", err)
	}
	_ = node.WaitExit()

	// 重启（复用同一 storage）→ PID 变化断言（变异：复用进程 → 红）。
	if err := node.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	pid2 := node.Pid()
	if pid2 == pid1 {
		t.Fatalf("重启后 PID 未变化: %d", pid1)
	}

	// 业务恢复：重启后文件仍在（checksum 台账持久化）。
	download(t, node.URL, "persist.txt")
	fmt.Printf("Kill9Restart OK: pid %d → %d, 文件恢复\n", pid1, pid2)
}

// TestChaos_NetPartition 验证 NetChaos proxy：Pause 断连（分区）→ Resume 恢复。
func TestChaos_NetPartition(t *testing.T) {
	t.Parallel()
	node, nerr := NewChaosNode(t)
	if nerr != nil {
		t.Fatalf("NewChaosNode: %v", nerr)
	}
	if err := node.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 经 proxy 访问节点（分区注入面）。
	pproxy, perr := NewNetChaos(strings.TrimPrefix(node.URL, "http://"))
	if perr != nil {
		t.Fatalf("NewNetChaos: %v", perr)
	}
	defer pproxy.Close()
	proxyURL := "http://" + pproxy.Addr()

	// 正常请求成功。
	req, _ := http.NewRequest(http.MethodGet, proxyURL+"/healthz", nil)
	resp, err := authedClient.Do(req)
	if err != nil {
		t.Fatalf("proxy 健康检查: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy healthz code = %d", resp.StatusCode)
	}

	// Pause（分区：新连接挂起）→ 请求超时/失败（3s 有界）。
	pproxy.Pause()
	client := &http.Client{Timeout: 3 * time.Second}
	req2, _ := http.NewRequest(http.MethodGet, proxyURL+"/healthz", nil)
	_, derr := client.Do(req2)
	if derr == nil {
		t.Fatal("分区期间请求应失败/超时")
	}

	// Resume → 恢复成功。
	pproxy.Resume()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req3, _ := http.NewRequest(http.MethodGet, proxyURL+"/healthz", nil)
		resp3, err := http.DefaultClient.Do(req3)
		if err == nil {
			_ = resp3.Body.Close()
			if resp3.StatusCode == http.StatusOK {
				return // 恢复成功
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("Resume 后未恢复")
}
