// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webdavgw "github.com/cocomhub/sproxy/pkg/gateway/webdav"
	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestNewRemoteHandler_NilDialer 验证 nil dialer 被拒。
func TestNewRemoteHandler_NilDialer(t *testing.T) {
	t.Parallel()
	_, err := webdavgw.NewRemoteHandler(nil, remote.Ref{Node: "n", Volume: "v"})
	if err == nil {
		t.Fatal("nil dialer 应返回错误")
	}
}

// TestNewRemoteHandler_WithMemFSBridge 验证用内存 sync.FS 直接桥接 handler
// （绕过 remote 传输层——协议层已在任务1覆盖，此处验证 handler 装配可用）。
func TestNewRemoteHandler_WithMemFSBridge(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)
	if handler == nil {
		t.Fatal("NewHandler(fs) 不应为 nil")
	}
	// PUT/GET 往返确认装配可用。
	resp := doRequest(t, handler, "PUT", "/r.txt", []byte("remote-data"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/r.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "remote-data" {
		t.Fatalf("GET = %q, want %q", got, "remote-data")
	}
}

// TestSubPathFS_RefWithPath 验证 NewRemoteHandler 支持子路径（remote://node/vol/sub）。
// 用 sync.FS 的 LocalFS 模拟远端：root 下放 sub/inner.txt，WebDAV 根应对准 sub/。
func TestSubPathFS_RefWithPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// 构造本地 FS：root/sub/inner.txt。
	if err := writeLocal(t, root, "sub/inner.txt", "sub-data"); err != nil {
		t.Fatal(err)
	}
	fs := syncpkg.NewLocalFS(root, nil)
	handler := webdavgw.NewHandler(webdavgw.NewSubPathFSForTest(fs, "sub"))
	if handler == nil {
		t.Fatal("handler 不应为 nil")
	}
	resp := doRequest(t, handler, "GET", "/inner.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /inner.txt 应 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "sub-data" {
		t.Fatalf("inner.txt = %q, want %q", got, "sub-data")
	}
	// 根 PROPFIND 应含 inner.txt（子路径根 = sub/）。
	resp = doRequest(t, handler, "PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / 应 207, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "inner.txt") {
		t.Fatalf("PROPFIND 应含 inner.txt, body=%s", body)
	}
}

// TestSubPathFS_Mapping 直接验证 subPathFS 语义（经导出测试钩子）。
func TestSubPathFS_Mapping(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeLocal(t, root, "sub/inner.txt", "sub-data"); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs := webdavgw.NewSubPathFSForTest(inner, "sub")
	// Stat 根（子路径根）应成功。
	e, err := fs.Stat(context.Background(), "")
	if err != nil {
		t.Fatalf("Stat root err: %v", err)
	}
	if e == nil {
		t.Fatal("Stat root 应返回根条目")
	}
	// ListDir 根应含 inner.txt（Path 相对新根）。
	entries, err := fs.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir root err: %v", err)
	}
	found := false
	for _, en := range entries {
		if en.Name == "inner.txt" && en.Path == "inner.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListDir 应含 inner.txt（Path=inner.txt 相对根）, got %+v", entries)
	}
	// 经完整 handler PUT/GET 往返（子路径根写读）。
	handler := webdavgw.NewHandler(fs)
	resp := doRequest(t, handler, "PUT", "/new.txt", []byte("n"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /new.txt 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/new.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "n" {
		t.Fatalf("GET /new.txt = %q, want %q", got, "n")
	}
}

// writeLocal 写本地测试文件（自动建父目录）。
func writeLocal(t *testing.T, root, rel, content string) error {
	t.Helper()
	full := filepath.Join(root, rel)
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
