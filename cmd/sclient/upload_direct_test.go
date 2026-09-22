// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"io"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestDirectUpload_FullFlow 验证 upload-direct 全流程：签发 → 直传 S3 → 登记。
func TestDirectUpload_FullFlow(t *testing.T) {
	t.Parallel()
	// mock S3：接收 PUT（直传）并记录。
	var s3Got string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			s3Got = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s3.Close()

	// mock sproxy：presign 返回指向 mock S3 的 URL；complete 返回 ok。
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

	// 临时文件。
	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(local, []byte("hello direct"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	svc := client.NewFileClient(srv.URL, client.WithSendNoAuth(true))
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	if err := directUpload(context.Background(), svc, "s3", local, "dir/a.txt", ios); err != nil {
		t.Fatalf("directUpload: %v", err)
	}
	if s3Got != "/dir/a.txt" {
		t.Fatalf("S3 收到 PUT 路径 = %q, want /dir/a.txt", s3Got)
	}
}

// TestDirectUpload_PresignError 签发失败（后端未注册 → 404）→ 错误传播。
func TestDirectUpload_PresignError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "后端类型未注册: s3", http.StatusNotFound)
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
		t.Fatal("签发失败应返回错误")
	}
}
