// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestReadBatchOpsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "ops.txt")
	content := "delete a.txt\n\n# comment\nmkdir dir1\n  rmdir dir2  \n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ops, err := readBatchOpsFile(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"delete a.txt", "mkdir dir1", "rmdir dir2"}
	if len(ops) != len(want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("ops[%d] = %q, want %q", i, ops[i], want[i])
		}
	}
}

func TestReadBatchOpsFile_MissingFile(t *testing.T) {
	t.Parallel()
	if _, err := readBatchOpsFile(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("期望文件不存在时报错")
	}
}

func TestNewCmdBatch_Registered(t *testing.T) {
	t.Parallel()
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdBatch(factory, cli.IOStreams{Out: io.Discard}, st)
	if cmd.Use != "batch <file>" {
		t.Errorf("batchCmd.Use = %q, want %q", cmd.Use, "batch <file>")
	}
	if w := cmd.Flags().Lookup("workers"); w == nil || w.DefValue != "4" {
		t.Errorf("batch --workers 默认值应为 4: %+v", w)
	}
}

// TestBatchCmd_DeleteOps 用 mock 服务端验证 batch 命令整链路：
// batch 文件 → runBatchConcurrent 并发 → printBatchResults 保序输出。
// mock 只处理 stat/delete/mkdir/rmdir/meta 的路径。
func TestBatchCmd_DeleteOps(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/stat":
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
		case "/delete", "/mkdir", "/rmdir":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success":true,"message":"ok"}`))
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}

	dir := t.TempDir()
	f := filepath.Join(dir, "ops.txt")
	if err := os.WriteFile(f, []byte("delete a.txt\nmkdir dir1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	cmd := NewCmdBatch(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{f})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("batch 命令失败: %v\n输出: %s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[OK] delete a.txt") || !strings.Contains(out, "[OK] mkdir dir1") {
		t.Errorf("输出应含两个 OK 结果（按行序）: %s", out)
	}
}

// TestBatchCmd_FailExitCode 验证任一行 FAIL 时命令返回非 nil error（退出码非 0）。
func TestBatchCmd_FailExitCode(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}

	dir := t.TempDir()
	f := filepath.Join(dir, "ops.txt")
	if err := os.WriteFile(f, []byte("delete a.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	cmd := NewCmdBatch(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{f})
	if err := cmd.Execute(); err == nil {
		t.Error("期望任一行 FAIL 时命令返回错误（非 0 退出码）")
	}
	if !strings.Contains(buf.String(), "[FAIL] delete a.txt") {
		t.Errorf("输出应含 FAIL 结果: %s", buf.String())
	}
}
