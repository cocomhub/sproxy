// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFileClient_OpenDownload_Stream(t *testing.T) {
	srv, dir := newMockServer(t)
	payload := []byte("streaming download body for sync")
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(srv.URL)
	rc, err := c.OpenDownload(context.Background(), "data.bin")
	if err != nil {
		t.Fatalf("OpenDownload error: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("下载内容不符: got %q, want %q", got, payload)
	}
}

func TestFileClient_OpenDownload_NotFound(t *testing.T) {
	srv, _ := newMockServer(t)
	c := NewFileClient(srv.URL)
	rc, err := c.OpenDownload(context.Background(), "missing.bin")
	if err == nil {
		_ = rc.Close()
		t.Fatalf("OpenDownload 应返回错误（文件不存在）")
	}
	if rc != nil {
		t.Fatalf("错误路径不应返回非 nil body")
	}
}

func TestFileClient_OpenDownload_PathTraversal(t *testing.T) {
	srv, _ := newMockServer(t)
	c := NewFileClient(srv.URL)
	if _, err := c.OpenDownload(context.Background(), "../etc/passwd"); err == nil {
		t.Fatalf("路径穿越应被拒绝")
	}
}
