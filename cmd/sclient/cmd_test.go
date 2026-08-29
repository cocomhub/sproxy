// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
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
	"github.com/spf13/cobra"
)

// ---- root command ----

func TestRootCmd_Use(t *testing.T) {
	cmd := NewRootCmd()
	if cmd.Use != "sclient" {
		t.Errorf("rootCmd.Use = %q, want %q", cmd.Use, "sclient")
	}
}

func TestRootCmd_SubCommands(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	ios := cli.IOStreams{Out: io.Discard}

	// 验证关键命令可通过工厂函数正确创建
	cmds := []struct {
		name string
		cmd  *cobra.Command
	}{
		{"upload", NewCmdUpload(factory, ios, st)},
		{"download", NewCmdDownload(factory, ios, st)},
		{"delete", NewCmdDelete(factory, ios, st)},
		{"list", NewCmdList(factory, ios, st)},
		{"search", NewCmdSearch(factory, ios)},
		{"version", NewCmdVersion(ios)},
		{"stats", NewCmdStats(factory, ios)},
		{"diag", NewCmdDiag(ios)},
		{"relay", NewCmdRelay(factory, ios, nil)},
		{"archive", NewCmdArchive(factory, ios)},
		{"batch-delete", NewCmdBatchDelete(factory, ios, st)},
		{"batch-rename", NewCmdBatchRename(factory, ios)},
		{"mv", NewCmdMv(factory, ios, st)},
		{"stat", NewCmdStat(factory, ios, &testConfigProvider{cfg: client.DefaultConfig()})},
		{"genkey", NewCmdGenkey(ios)},
		{"tunnel", NewCmdTunnel(factory, ios)},
		{"cd", NewCmdCd(st, ios)},
		{"pwd", NewCmdPwd(st, ios)},
		{"mkdir", NewCmdMkdir(factory, ios, st)},
	}
	for _, c := range cmds {
		if !strings.HasPrefix(c.cmd.Use, c.name) {
			t.Errorf("command %q: Use = %q, want prefix %q", c.name, c.cmd.Use, c.name)
		}
	}
}

func TestRootCmd_PersistentFlags(t *testing.T) {
	// 验证 persistent flags 已注册
	cmd := NewRootCmd()
	flagNames := []string{"config", "server", "output", "verbose", "chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := cmd.PersistentFlags().Lookup(name)
		if f == nil {
			t.Errorf("missing persistent flag: %q", name)
		}
	}
}

// ---- upload command ----

func TestUploadCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "upload <file1> [file2...]" {
		t.Errorf("uploadCmd.Use = %q", cmd.Use)
	}
	if cmd.Short != "上传一个或多个文件" {
		t.Errorf("uploadCmd.Short = %q", cmd.Short)
	}
	// MinimumNArgs(1)
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("upload should require at least 1 arg")
	}
	if err := cmd.Args(cmd, []string{"a.txt"}); err != nil {
		t.Errorf("upload with 1 arg should be ok: %v", err)
	}
	// 验证 flags
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("uploadCmd missing flag: %q", name)
		}
	}
}

// ---- download command ----

func TestDownloadCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDownload(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "download <filename> [output]" {
		t.Errorf("downloadCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("download should require at least 1 arg")
	}
	if err := cmd.Args(cmd, []string{"file.txt"}); err != nil {
		t.Errorf("download with 1 arg should be ok: %v", err)
	}
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("downloadCmd missing flag: %q", name)
		}
	}
}

// ---- delete command ----

func TestDeleteCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDelete(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "delete <filename>" {
		t.Errorf("deleteCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("delete should require exactly 1 arg")
	}
	if err := cmd.Args(cmd, []string{"a.txt"}); err != nil {
		t.Errorf("delete with 1 arg should be ok: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("delete should reject 2 args")
	}
	f := cmd.Flags().Lookup("check-local")
	if f == nil {
		t.Error("deleteCmd missing flag: check-local")
	}
}

// ---- list command ----

func TestListCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdList(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "list" {
		t.Errorf("listCmd.Use = %q", cmd.Use)
	}
	f := cmd.Flags().Lookup("subdir")
	if f == nil {
		t.Error("listCmd missing flag: subdir")
	}
}

// ---- relay command ----

func TestRelayCmd_Registered(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdRelay(factory, cli.IOStreams{Out: io.Discard}, nil)
	if cmd.Use != "relay" {
		t.Errorf("relayCmd.Use = %q, want 'relay'", cmd.Use)
	}
}

// ---- diag command ----

func TestDiagCmd_Registered(t *testing.T) {
	cmd := NewCmdDiag(cli.IOStreams{Out: io.Discard})
	if cmd.Use != "diag" {
		t.Errorf("diagCmd.Use = %q, want 'diag'", cmd.Use)
	}
}

// ---- batch-delete command ----

func TestBatchDeleteCmd_Registered(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdBatchDelete(factory, cli.IOStreams{Out: io.Discard}, st)
	if cmd.Use != "batch-delete <file1> [file2...]" {
		t.Errorf("batchDeleteCmd.Use = %q", cmd.Use)
	}
}

// ---- archive command ----

func TestArchiveCmd_Registered(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdArchive(factory, cli.IOStreams{Out: io.Discard})
	if cmd.Use != "archive [flags] <file...>" {
		t.Errorf("archiveCmd.Use = %q", cmd.Use)
	}
}

// ---- writeArchiveResponse ----

func TestWriteArchiveResponse(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("archive-content"))
	}))
	defer mock.Close()

	resp, err := http.Get(mock.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	dst := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := writeArchiveResponse(resp, dst); err != nil {
		t.Fatalf("writeArchiveResponse: %v", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "archive-content" {
		t.Errorf("got %q, want archive-content", string(data))
	}
}

// ---- mv command ----

func TestMvCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "mv <from> <to>" {
		t.Errorf("mvCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("mv should require exactly 2 args")
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err != nil {
		t.Errorf("mv with 2 args should be ok: %v", err)
	}
}

// ---- stat command ----

func TestStatCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdStat(factory, cli.IOStreams{Out: io.Discard}, &testConfigProvider{cfg: client.DefaultConfig()})

	if cmd.Use != "stat [server]" {
		t.Errorf("statCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("stat with no args should be ok: %v", err)
	}
	if err := cmd.Args(cmd, []string{"server"}); err != nil {
		t.Errorf("stat with 1 arg should be ok: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("stat should reject 2 args")
	}
}

// ---- search command ----

func TestSearchCmd(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdSearch(factory, cli.IOStreams{Out: io.Discard})

	if cmd.Use != "search <keyword>" {
		t.Errorf("searchCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("search should require exactly 1 arg")
	}
	if err := cmd.Args(cmd, []string{"keyword"}); err != nil {
		t.Errorf("search with 1 arg should be ok: %v", err)
	}
}

// ---- version command ----

func TestVersionCmd(t *testing.T) {
	cmd := NewCmdVersion(cli.IOStreams{Out: io.Discard})
	if !strings.HasPrefix(cmd.Use, "version") {
		t.Errorf("versionCmd.Use = %q, want prefix 'version'", cmd.Use)
	}
}

// ---- initLogger ----

func TestCLientInitLogger(t *testing.T) {
	logger := initLogger(false)
	if logger == nil {
		t.Fatal("initLogger returned nil")
	}

	// verbose mode
	verboseLogger := initLogger(true)
	if verboseLogger == nil {
		t.Fatal("initLogger(true) returned nil")
	}

	// level: verbose -> debug
	verboseHandler, ok := verboseLogger.Handler().(*slog.TextHandler)
	if !ok {
		t.Log("handler is not TextHandler, skipping level check")
		return
	}
	_ = verboseHandler // 实际 level 无法从 Handler 直接读取，仅验证不崩溃
}

// ---- helper tests ----

func TestResolveRemotePathOrErr(t *testing.T) {
	st := &state.State{CurrentDir: ""}
	got, err := st.ResolveRemotePathOrErr("test.txt")
	if err != nil {
		t.Fatalf("ResolveRemotePathOrErr('test.txt') unexpected error: %v", err)
	}
	if got != "test.txt" {
		t.Errorf("ResolveRemotePathOrErr('test.txt') = %q, want 'test.txt'", got)
	}
}

// ---- Upload command RunE 测试 ----

func TestUploadCommand(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"Success":true,"Message":"uploaded"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{srcFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upload command failed: %v", err)
	}
}

// ---- Download command RunE 测试 ----

func TestDownloadCommand_Success(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.txt")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// download command 的 Stat 调用
		if r.URL.Path == "/stat" {
			w.Header().Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
		w.Header().Set("X-File-MTime", "0")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"test.txt", dst})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("download command failed: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("expected hello, got %s", string(data))
	}
}

// ---- Delete command RunE 测试 ----

func TestDeleteCommand_Success(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"Success":true,"Message":"deleted"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDelete(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"test.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete command failed: %v", err)
	}
}

// ---- List command RunE 测试 ----

func TestListCommand(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[{"name":"a.txt","size":10}],"total":1}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdList(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list command failed: %v", err)
	}
}

func TestListCommand_WithSubdirFlag(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[{"name":"sub/","size":0,"is_dir":true}],"total":1}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdList(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"/sub"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list command with subdir failed: %v", err)
	}
}

// ---- 已迁移完 ----
// captureRootCmdArgs 已删除，所有测试已迁移到工厂函数模式。

// ---- Error path tests ----

func TestUploadCommand_FileNotFound(t *testing.T) {
	// 使用真实的 HTTP 客户端但指向无法连接的地址
	svc := client.NewFileClient("http://127.0.0.1:1")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"/nonexistent/file.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestDeleteCommand_ChecksumMismatch(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"success":false,"message":"checksum mismatch"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdDelete(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for checksum mismatch")
	}
}

func TestListCommand_EmptyResult(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[],"total":0}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdList(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "no files found") {
		t.Errorf("expected 'no files found', got: %s", buf.String())
	}
}

// ---- rmdir command ----

func TestRmdirCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRmdir(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &state.State{})
	if cmd.Use != "rmdir <dirname>" {
		t.Errorf("rmdirCmd.Use = %q", cmd.Use)
	}
}

func TestRmdirCmd_HappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"deleted"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdRmdir(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"mydir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rmdir command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "目录已删除") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
}

func TestRmdirCmd_Force(t *testing.T) {
	// --force 是客户端 flag，用于跳过非空目录的确认提示（不传递给服务端 API）
	// 测试场景：目录非空 + --force 应直接删除，不需要交互确认
	listCalled := false
	rmdirCalled := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files" {
			listCalled = true
			// 返回非空目录，触发非空检查
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"files":[{"name":"file.txt","size":100}]}`))
			return
		}
		if r.Method == "POST" && r.URL.Path == "/rmdir" {
			rmdirCalled = true
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success":true,"message":"deleted"}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdRmdir(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"--force", "mydir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("rmdir --force command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "目录已删除") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
	if !listCalled {
		t.Error("expected List to be called for non-empty check")
	}
	if !rmdirCalled {
		t.Error("expected Rmdir to be called")
	}
}

func TestRmdirCmd_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdRmdir(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"mydir"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- mv command additional tests ----

func TestMvCommand_StatNotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	var buf, errBuf strings.Builder
	cmd := NewCmdMv(factory, cli.IOStreams{Out: &buf, ErrOut: &errBuf}, st)
	cmd.SetArgs([]string{"nonexistent.txt", "new.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when stat returns 404")
	}
}

func TestMvCommand_ChecksumEmpty(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "")
		w.Header().Set("X-File-Size", "0")
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"old.txt", "new.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when checksum is empty")
	}
}

func TestMvCommand_PathTraversal(t *testing.T) {
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"../outside.txt", "new.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestMvCommand_Args(t *testing.T) {
	t.Parallel()
	factory := clientfactory.NewMock(nil, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard}, st)
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("mv should require exactly 2 args")
	}
	if err := cmd.Args(cmd, []string{"a.txt"}); err == nil {
		t.Error("mv should reject 1 arg")
	}
	if err := cmd.Args(cmd, []string{"a", "b", "c"}); err == nil {
		t.Error("mv should reject 3 args")
	}
}
