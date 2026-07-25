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
		{"version", NewCmdVersion(factory, ios)},
		{"stats", NewCmdStats(factory, ios)},
		{"diag", NewCmdDiag(ios)},
		{"relay", NewCmdRelay(factory, ios)},
		{"archive", NewCmdArchive(factory, ios)},
		{"batch-delete", NewCmdBatchDelete(factory, ios, st)},
		{"batch-rename", NewCmdBatchRename(factory, ios)},
		{"mv", NewCmdMv(factory, ios, st)},
		{"stat", NewCmdStat(factory, ios, st)},
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
	cmd := NewCmdRelay(factory, cli.IOStreams{Out: io.Discard})
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
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdStat(factory, cli.IOStreams{Out: io.Discard}, st)

	if cmd.Use != "stat <filename>" {
		t.Errorf("statCmd.Use = %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("stat should require exactly 1 arg")
	}
	if err := cmd.Args(cmd, []string{"f.txt"}); err != nil {
		t.Errorf("stat with 1 arg should be ok: %v", err)
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
	factory := clientfactory.NewMock(nil, nil)
	cmd := NewCmdVersion(factory, cli.IOStreams{Out: io.Discard})
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
