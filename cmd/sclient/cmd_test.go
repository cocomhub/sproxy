// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/spf13/cobra"
)

// ---- root command ----

func TestRootCmd_Use(t *testing.T) {
	if rootCmd.Use != "sclient" {
		t.Errorf("rootCmd.Use = %q, want %q", rootCmd.Use, "sclient")
	}
}

func TestRootCmd_SubCommands(t *testing.T) {
	cmds := rootCmd.Commands()
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Use
	}
	for _, want := range []string{"upload", "download", "delete", "list", "search", "tunnel", "genkey", "config", "version"} {
		found := false
		for _, name := range names {
			if strings.HasPrefix(name, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing subcommand: %q in %v", want, names)
		}
	}
}

func TestRootCmd_PersistentFlags(t *testing.T) {
	// 验证 persistent flags 已注册
	flagNames := []string{"config", "server", "output", "verbose", "chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			t.Errorf("missing persistent flag: %q", name)
		}
	}
}

// ---- upload command ----

func TestUploadCmd(t *testing.T) {
	if uploadCmd.Use != "upload <file1> [file2...]" {
		t.Errorf("uploadCmd.Use = %q", uploadCmd.Use)
	}
	if uploadCmd.Short != "上传一个或多个文件" {
		t.Errorf("uploadCmd.Short = %q", uploadCmd.Short)
	}
	// MinimumNArgs(1)
	if err := uploadCmd.Args(uploadCmd, []string{}); err == nil {
		t.Error("upload should require at least 1 arg")
	}
	if err := uploadCmd.Args(uploadCmd, []string{"a.txt"}); err != nil {
		t.Errorf("upload with 1 arg should be ok: %v", err)
	}
	// 验证 flags
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := uploadCmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("uploadCmd missing flag: %q", name)
		}
	}
}

// ---- download command ----

func TestDownloadCmd(t *testing.T) {
	if downloadCmd.Use != "download <filename> [output]" {
		t.Errorf("downloadCmd.Use = %q", downloadCmd.Use)
	}
	if err := downloadCmd.Args(downloadCmd, []string{}); err == nil {
		t.Error("download should require at least 1 arg")
	}
	if err := downloadCmd.Args(downloadCmd, []string{"file.txt"}); err != nil {
		t.Errorf("download with 1 arg should be ok: %v", err)
	}
	flagNames := []string{"chunked", "chunk-size", "concurrency", "resume"}
	for _, name := range flagNames {
		f := downloadCmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("downloadCmd missing flag: %q", name)
		}
	}
}

// ---- delete command ----

func TestDeleteCmd(t *testing.T) {
	if deleteCmd.Use != "delete <filename>" {
		t.Errorf("deleteCmd.Use = %q", deleteCmd.Use)
	}
	if err := deleteCmd.Args(deleteCmd, []string{}); err == nil {
		t.Error("delete should require exactly 1 arg")
	}
	if err := deleteCmd.Args(deleteCmd, []string{"a.txt"}); err != nil {
		t.Errorf("delete with 1 arg should be ok: %v", err)
	}
	if err := deleteCmd.Args(deleteCmd, []string{"a", "b"}); err == nil {
		t.Error("delete should reject 2 args")
	}
	f := deleteCmd.Flags().Lookup("check-local")
	if f == nil {
		t.Error("deleteCmd missing flag: check-local")
	}
}

// ---- list command ----

func TestListCmd(t *testing.T) {
	if listCmd.Use != "list" {
		t.Errorf("listCmd.Use = %q", listCmd.Use)
	}
	f := listCmd.Flags().Lookup("subdir")
	if f == nil {
		t.Error("listCmd missing flag: subdir")
	}
}

// ---- relay command ----

func TestRelayCmd_Registered(t *testing.T) {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "relay" {
			return
		}
	}
	t.Error("relay command not registered")
}

// ---- diag command ----

func TestDiagCmd_Registered(t *testing.T) {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "diag" {
			return
		}
	}
	t.Error("diag command not registered")
}

// ---- batch-delete command ----

func TestBatchDeleteCmd_Registered(t *testing.T) {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "batch-delete" {
			return
		}
	}
	t.Error("batch-delete command not registered")
}

// ---- archive command ----

func TestArchiveCmd_Registered(t *testing.T) {
	for _, sub := range rootCmd.Commands() {
		if sub.Name() == "archive" {
			return
		}
	}
	t.Error("archive command not registered")
}

// ---- saveCurrentDir ----

func TestSaveCurrentDir(t *testing.T) {
	old := currentDir
	t.Cleanup(func() { currentDir = old })
	currentDir = "/test/dir"
	saveCurrentDir()
}

// ---- writeArchiveResponse ----

func TestWriteArchiveResponse(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("archive-content"))
	}))
	defer mock.Close()

	resp, _ := http.Get(mock.URL)
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
	if mvCmd.Use != "mv <from> <to>" {
		t.Errorf("mvCmd.Use = %q", mvCmd.Use)
	}
	if err := mvCmd.Args(mvCmd, []string{}); err == nil {
		t.Error("mv should require exactly 2 args")
	}
	if err := mvCmd.Args(mvCmd, []string{"a", "b"}); err != nil {
		t.Errorf("mv with 2 args should be ok: %v", err)
	}
}

// ---- stat command ----

func TestStatCmd(t *testing.T) {
	if statCmd.Use != "stat <filename>" {
		t.Errorf("statCmd.Use = %q", statCmd.Use)
	}
	if err := statCmd.Args(statCmd, []string{}); err == nil {
		t.Error("stat should require exactly 1 arg")
	}
	if err := statCmd.Args(statCmd, []string{"f.txt"}); err != nil {
		t.Errorf("stat with 1 arg should be ok: %v", err)
	}
}

// ---- search command ----

func TestSearchCmd(t *testing.T) {
	if searchCmd.Use != "search <keyword>" {
		t.Errorf("searchCmd.Use = %q", searchCmd.Use)
	}
	if err := searchCmd.Args(searchCmd, []string{}); err == nil {
		t.Error("search should require exactly 1 arg")
	}
	if err := searchCmd.Args(searchCmd, []string{"keyword"}); err != nil {
		t.Errorf("search with 1 arg should be ok: %v", err)
	}
}

// ---- version command ----

func TestVersionCmd(t *testing.T) {
	// version command is not registered via init() but via rootCmd init directly
	var found bool
	for _, c := range rootCmd.Commands() {
		if strings.HasPrefix(c.Use, "version") {
			found = true
			break
		}
	}
	if !found {
		t.Error("version command not registered in rootCmd")
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
	old := currentDir
	currentDir = ""
	defer func() { currentDir = old }()

	got, err := resolveRemotePathOrErr("test.txt")
	if err != nil {
		t.Fatalf("resolveRemotePathOrErr('test.txt') unexpected error: %v", err)
	}
	if got != "test.txt" {
		t.Errorf("resolveRemotePathOrErr('test.txt') = %q, want 'test.txt'", got)
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

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"upload", "--server", mock.URL, srcFile})
	if err := rootCmd.Execute(); err != nil {
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

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"download", "--server", mock.URL, "test.txt", dst})
	if err := rootCmd.Execute(); err != nil {
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

	resetState := captureRootCmdArgs()
	defer resetState()

	// 使用绝对路径绕过 currentDir
	rootCmd.SetArgs([]string{"delete", "--server", mock.URL, "/test.txt"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("delete command failed: %v", err)
	}
}

// ---- List command RunE 测试 ----

func TestListCommand(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[{"name":"a.txt","size":10}],"total":1}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"list", "--server", mock.URL})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("list command failed: %v", err)
	}
}

func TestListCommand_WithSubdirFlag(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[{"name":"sub/","size":0,"is_dir":true}],"total":1}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	// --subdir 使用绝对路径绕过 currentDir
	rootCmd.SetArgs([]string{"list", "--server", mock.URL, "--subdir", "/sub"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("list command with subdir failed: %v", err)
	}
}

// ---- 共享辅助函数 ----

// captureRootCmdArgs 保存并重置 rootCmd 的 args、PersistentPreRunE 状态，
// 以及全局 cfgFile、cfgProvider、currentDir。
// 返回的恢复函数应在测试结束时 defer 调用。
func captureRootCmdArgs() func() {
	oldArgs := rootCmd.Args
	oldPreRunE := rootCmd.PersistentPreRunE
	oldCurrentDir := currentDir
	oldCfgFile := cfgFile
	oldCfgProvider := cfgProvider
	currentDir = ""
	cfgProvider = nil

	// 替换 PersistentPreRunE 为简化版：初始化 cfgProvider 但不触发 loadCurrentDir
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfgProvider = sclientcfg.New(cfgFile)
		cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup(flagServer))
		cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup(flagChunkSize))
		// 不调用 loadCurrentDir()，防止从磁盘覆盖测试设置的 currentDir
		return nil
	}

	rootCmd.SetArgs(nil)
	return func() {
		rootCmd.Args = oldArgs
		rootCmd.PersistentPreRunE = oldPreRunE
		currentDir = oldCurrentDir
		cfgFile = oldCfgFile
		cfgProvider = oldCfgProvider
	}
}

// 重置 cobra.Command 的 help func 避免在测试中意外触发帮助输出
func init() {
	// 不修改生产代码，为测试预设一些状态
}
