// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// newBackupCmdMock 返回 mock 服务端（GET /api/volumes/export 流式 tar）。
func newBackupCmdMock(t *testing.T, tarBytes []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/volumes/export" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// backupCmdTar 构造最小合法 tar（user/a.txt + 尾部 manifest.json）。
func backupCmdTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "user/a.txt", Mode: 0o644, Size: 5}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("写 tar 头失败: %v", err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatalf("写 tar 内容失败: %v", err)
	}
	manifest := []byte(`{"volume":"disk2","files":[{"path":"user/a.txt","size":5}]}`)
	mhdr := &tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}
	if err := tw.WriteHeader(mhdr); err != nil {
		t.Fatalf("写 manifest 头失败: %v", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatalf("写 manifest 内容失败: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// TestBackupCmd_HappyPath 断言：backup <vol> <dest> 调 GET /api/volumes/export?volume=vol
// 并把响应流式落盘到 dest（输出含卷名与目标路径）。
func TestBackupCmd_HappyPath(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	var gotVol string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes/export" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotVol = r.URL.Query().Get("volume")
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(backupCmdTar(t))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	dest := filepath.Join(t.TempDir(), "disk2.tar")
	var buf strings.Builder
	cmd := NewCmdBackup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"disk2", dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("backup 命令失败: %v", err)
	}
	if gotVol != "disk2" {
		t.Errorf("export volume query = %q, want disk2", gotVol)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读备份文件失败: %v", err)
	}
	if !bytes.Equal(data, backupCmdTar(t)) {
		t.Errorf("备份文件内容与导出流不一致（len=%d）", len(data))
	}
	if !strings.Contains(buf.String(), "备份完成") || !strings.Contains(buf.String(), dest) {
		t.Errorf("输出缺成功文案：%s", buf.String())
	}
}

// TestBackupCmd_EmptyVolume 断言：backup "" <dest> 不携带 volume 参数（全卷视图）。
func TestBackupCmd_EmptyVolume(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("volume"); got != "" {
			t.Errorf("空卷导出不应携带 volume 参数，got %q", got)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(backupCmdTar(t))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	dest := filepath.Join(t.TempDir(), "all.tar")
	cmd := NewCmdBackup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"", dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("backup 空卷失败: %v", err)
	}
}

// TestBackupCmd_OutputFlag 断言：-o <path> 指定输出（第三参数形态）。
func TestBackupCmd_OutputFlag(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := newBackupCmdMock(t, backupCmdTar(t))
	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	dest := filepath.Join(t.TempDir(), "flag.tar")
	cmd := NewCmdBackup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"-o", dest, "disk2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("backup -o 失败: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("目标文件未生成: %v", err)
	}
}

// TestBackupCmd_ServerError 断言：服务端 403 → 命令报错（不写目标文件）。
func TestBackupCmd_ServerError(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "volume not allowed", http.StatusForbidden)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	dest := filepath.Join(t.TempDir(), "disk2.tar")
	cmd := NewCmdBackup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"disk2", dest})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("403 应报错")
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Errorf("导出失败不应生成目标文件（Stat err=%v）", serr)
	}
}

// TestBackupCmd_MissingArgs 断言：无参数 → 参数校验错误（cobra MinimumNArgs(1)）。
func TestBackupCmd_MissingArgs(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	cmd := NewCmdBackup(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("backup 至少需要 1 个参数（<vol>），got nil")
	}
	if err := cmd.Args(cmd, []string{"disk2"}); err != nil {
		t.Errorf("backup 1 参（<vol> + -o）应合法: %v", err)
	}
}

// TestBackupCmd_MissingDest 断言：<vol> 无 <dest> 且未给 -o → 明确报错（防误写）。
func TestBackupCmd_MissingDest(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1")
	factory := clientfactory.NewMock(svc, nil)
	var errBuf strings.Builder
	cmd := NewCmdBackup(factory, cli.IOStreams{Out: io.Discard, ErrOut: &errBuf})
	cmd.SetArgs([]string{"disk2"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("缺 <dest> 且无 -o 应报错")
	}
	if !strings.Contains(err.Error(), "目标文件路径不能为空") {
		t.Errorf("错误信息应说明目标路径缺失，got: %v", err)
	}
}
