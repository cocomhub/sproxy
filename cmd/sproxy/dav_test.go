// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	webdavgw "github.com/cocomhub/sproxy/pkg/gateway/webdav"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/remote"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/spf13/cobra"
)

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

// TestDavCommand_Registered 验证 dav 子命令已注册到 rootCmd。
func TestDavCommand_Registered(t *testing.T) {
	t.Parallel()
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Use == "dav [remote://node/vol[/path]]" || strings.HasPrefix(c.Use, "dav ") {
			found = true
			if c.Short == "" {
				t.Fatal("dav 子命令应有 Short 描述")
			}
		}
	}
	if !found {
		t.Fatal("dav 子命令未注册到 rootCmd")
	}
}

// TestDavCommand_ParseRef 验证 remote:// 句柄解析（dav 命令第一步）。
func TestDavCommand_ParseRef(t *testing.T) {
	t.Parallel()
	ref, err := remote.ParseRef("remote://nodeA/main")
	if err != nil {
		t.Fatalf("ParseRef 失败: %v", err)
	}
	if ref.Node != "nodeA" || ref.Volume != "main" {
		t.Fatalf("ref = %+v, want nodeA/main", ref)
	}
	// 子路径。
	ref2, err := remote.ParseRef("remote://nodeA/main/sub/dir")
	if err != nil {
		t.Fatalf("ParseRef 子路径失败: %v", err)
	}
	if ref2.Path != "sub/dir" {
		t.Fatalf("ref2.Path = %q, want sub/dir", ref2.Path)
	}
	// 非法句柄。
	if _, err := remote.ParseRef("https://node/vol"); err == nil {
		t.Fatal("非 remote:// 前缀应拒绝")
	}
}

// TestDavCommand_HandlerRoundtrip 验证 dav 命令的 handler 装配可经内存 FS 往返
// （协议层已由 pkg/gateway/webdav 测试覆盖；此处确认命令装配链可用）。
func TestDavCommand_HandlerRoundtrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeLocal(t, root, "hello.txt", "dav-hello"); err != nil {
		t.Fatal(err)
	}
	fs := syncpkg.NewLocalFS(root, nil)
	handler := webdavgw.NewHandler(fs)
	if handler == nil {
		t.Fatal("handler 不应为 nil")
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{Transport: netutil.IsolatedTransport()}
	defer client.CloseIdleConnections()

	// 经真实 HTTP 服务 GET。
	resp, err := client.Get(srv.URL + "/hello.txt")
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "dav-hello" {
		t.Fatalf("GET = %q, want %q", got, "dav-hello")
	}

	// PUT 往返。
	putReq, _ := http.NewRequest("PUT", srv.URL+"/new.txt", bytes.NewReader([]byte("new-data")))
	putResp, err := client.Do(putReq)
	if err != nil {
		t.Fatalf("PUT 失败: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT 应 201/204, got %d", putResp.StatusCode)
	}
	getResp, _ := client.Get(srv.URL + "/new.txt")
	got2, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if string(got2) != "new-data" {
		t.Fatalf("PUT 后 GET = %q, want %q", got2, "new-data")
	}
}

// 编译期断言：dav 命令用 cobra.Command（防误删 import）。
var _ = cobra.Command{}
