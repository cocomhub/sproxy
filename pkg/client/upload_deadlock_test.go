// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUpload_Tunnel_ServerDoesNotReadBody_NoDeadlock 复现并守护：
// 经过加密隧道上传时，若服务端在未消费完请求体的情况下提前返回（如 400），
// Upload 必须返回，不能因请求体加密 goroutine 阻塞在 io.Pipe 上而永久挂起 uploadWg.Wait()。
func TestUpload_Tunnel_ServerDoesNotReadBody_NoDeadlock(t *testing.T) {
	// 服务端立即返回 400，不读取请求体（模拟上游断流 / 快速失败）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadRequest)
	}))
	defer ts.Close()

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "big.bin")
	// 体积 > 单个加密块（64KB），确保请求体在传输中途被服务端“遗弃”。
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 256<<10)), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL,
		WithTunnel(testTunnelAK, testTunnelSK),
		WithTimeout(2*time.Second),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := c.Upload(ctx, src, "big.bin")
	// 无论成功与否，Upload 都必须返回；历史上此处会因请求体管道无人消费而死锁,
	// 由 go test -timeout 判定为“test timed out”。
	if err == nil {
		t.Log("上传意外成功（服务端返回 400，预期失败）")
	}
}
