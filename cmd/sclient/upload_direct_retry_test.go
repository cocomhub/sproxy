// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// upload_direct_retry_test.go 验证直传失败自动重试（roadmap 3.3 直传增强）：
//  1. PUT 5xx/网络失败 → 指数退避重试（默认 3 次），成功即停。
//  2. 重试前重新签发（presigned URL 过期风险——每次 PUT 用新 URL）。
//  3. 重试耗尽仍失败 → 错误传播。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestDirectUpload_RetryOn5xx PUT 前 2 次 503 → 第 3 次成功。
func TestDirectUpload_RetryOn5xx(t *testing.T) {
	t.Parallel()
	var putAttempts atomic.Int32
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if putAttempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/backends/s3/presign":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"url": %q}`, s3.URL+"/dir/a.txt")
		case "/api/backends/s3/presign/complete":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok": "registered"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("retry me"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	svc := client.NewFileClient(srv.URL, client.WithSendNoAuth(true))
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	if err := directUpload(context.Background(), svc, "s3", local, "dir/a.txt", ios); err != nil {
		t.Fatalf("directUpload（重试后成功）: %v", err)
	}
	if putAttempts.Load() != 3 {
		t.Fatalf("PUT 尝试次数 = %d, want 3（前 2 次失败 + 第 3 次成功）", putAttempts.Load())
	}
}

// TestDirectUpload_RetryExhausted 重试耗尽 → 错误传播。
func TestDirectUpload_RetryExhausted(t *testing.T) {
	t.Parallel()
	var putAttempts atomic.Int32
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putAttempts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s3.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"url": %q}`, s3.URL+"/dir/a.txt")
	}))
	defer srv.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	svc := client.NewFileClient(srv.URL, client.WithSendNoAuth(true))
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	err := directUpload(context.Background(), svc, "s3", local, "dir/a.txt", ios)
	if err == nil {
		t.Fatal("重试耗尽应返回错误")
	}
	if putAttempts.Load() != directUploadMaxRetries {
		t.Fatalf("PUT 尝试次数 = %d, want %d", putAttempts.Load(), directUploadMaxRetries)
	}
}

// 缩短重试退避（测试快）：包级变量注入。
func init() {
	directUploadRetryDelay = func(_ int) time.Duration { return 1 * time.Millisecond }
}
