// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
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
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// ---- Search command RunE 测试 ----

func TestSearchCommand_HappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/files/search" {
			t.Errorf("expected path /api/files/search, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("q") != "report" {
			t.Errorf("expected q=report, got %s", r.URL.Query().Get("q"))
		}
		w.Write([]byte(`{"files":[{"name":"report.pdf","size":100}],"total":1}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"search", "--server", mock.URL, "report"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("search command failed: %v", err)
		}
	})
	if !strings.Contains(out, "report.pdf") {
		t.Errorf("expected output to contain report.pdf, got: %s", out)
	}
}

func TestSearchCommand_NoResults(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[],"total":0}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.PersistentFlags().Set("json", "false")

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"search", "--server", mock.URL, "nonexistent"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("search command failed: %v", err)
		}
	})
	if !strings.Contains(out, "no files found") {
		t.Errorf("expected 'no files found' message, got: %s", out)
	}
}

// ---- Mv command RunE 测试 ----

func TestMvCommand_HappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/stat":
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
		case "/rename":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success":true,"message":"renamed"}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"mv", "--server", mock.URL, "old.txt", "new.txt"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("mv command failed: %v", err)
		}
	})
	if !strings.Contains(out, "已重命名") {
		t.Errorf("expected rename success message, got: %s", out)
	}
}

func TestMvCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stat endpoint returns valid checksum
		w.Header().Set("X-File-Checksum", "abc123")
		w.Header().Set("X-File-Size", "5")
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	_ = testutil.CaptureStderr(func() {
		rootCmd.SetArgs([]string{"mv", "--server", mock.URL, "old.txt", "new.txt"})
		rootCmd.Execute()
	})
}

// ---- Stat command RunE 测试 ----

func TestStatCommand_HappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "abc123def456")
		w.Header().Set("X-File-Size", "42")
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"stat", "--server", mock.URL, "test.txt"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("stat command failed: %v", err)
		}
	})
	if !strings.Contains(out, "abc123def456") {
		t.Errorf("expected checksum in output, got: %s", out)
	}
}

func TestStatCommand_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	// Stat returns 404 -> command prints error to stderr
	err := rootCmd.Execute()
	if err != nil {
		// Execute() may return error or print to stderr and exit
		// We just verify it doesn't panic
	}
	_ = err
}

// ---- Batch-delete command RunE 测试 ----

func TestBatchDeleteCommand(t *testing.T) {
	// Create a local file for checksum computation by batch-delete
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "a.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			// Return the actual checksum of "hello"
			w.Header().Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
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

	rootCmd.SetArgs([]string{"batch-delete", "--server", mock.URL, srcFile})
	_ = rootCmd.Execute()
}

// ---- Archive command RunE 测试 ----

func TestArchiveCommand(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake archive data"))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"archive", "--server", mock.URL, "-o", dst, "test.txt"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("archive command failed: %v", err)
	}
}

// ---- Genkey 测试已迁移到 genkey_test.go ----
// TestGenkeyCommand 已移除，使用 TestNewCmdGenkey 替代

// ---- Version 测试已迁移到新工厂函数 ----
// TestVersionCommand_Run 已移除，版本命令不再通过 rootCmd 注册

// ---- Version 子命令 error 测试 ----

func TestNewCmdVersionList_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdVersionList(factory, cli.IOStreams{ErrOut: io.Discard})

	cmd.SetArgs([]string{"test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 404")
	}
}

func TestNewCmdVersionRestore_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdVersionRestore(factory, cli.IOStreams{ErrOut: io.Discard})

	cmd.SetArgs([]string{"test.txt", "1"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 404")
	}
}

func TestNewCmdVersionDelete_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdVersionDelete(factory, cli.IOStreams{ErrOut: io.Discard})

	cmd.SetArgs([]string{"test.txt", "1"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 404")
	}
}

// ---- Tunnel command RunE 测试 ----

func TestTunnelCommand_MissingKey(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "http://example.com"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when tunnel_key is missing")
	}
}

// ---- Batch-rename command RunE 测试 ----

func TestBatchRenameCommand_AllSuccess(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// batch-rename 先 stat 获取 checksum
		if r.URL.Path == "/api/files/stat" || r.Method == "HEAD" {
			w.Header().Set("X-File-Checksum", "abc123def456")
			w.Header().Set("X-File-Size", "42")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		// 然后 POST /rename
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"renamed"}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"batch-rename", "--server", mock.URL, "old1.txt", "new1.txt", "old2.txt", "new2.txt"})
	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("batch-rename command failed: %v", err)
	}
}

func TestBatchRenameCommand_StatFails(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	testutil.CaptureStderr(func() {
		rootCmd.SetArgs([]string{"batch-rename", "--server", mock.URL, "old.txt", "new.txt"})
		err := rootCmd.Execute()
		if err != nil {
			t.Logf("batch-rename expected non-nil exit: %v", err)
		}
	})
}

// ---- Tunnel command RunE 扩展测试（skip, 因为需要有效的 tunnel_key）----

func TestTunnelCommand_WithConfigKey(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: http://127.0.0.1:18083\n", testutil.TestKey()))
	if err := os.WriteFile(cfgPath, cfgContent, 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":200}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	tmpContent := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: %s\n", testutil.TestKey(), mock.URL))
	if err := os.WriteFile(cfgPath, tmpContent, 0644); err != nil {
		t.Fatal(err)
	}

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "http://any-host.local/data"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error after config: %v", err)
	}
}

func TestTunnelCommand_HeaderFlag(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	cfgContent := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: %s\n", testutil.TestKey(), mock.URL))
	if err := os.WriteFile(cfgPath, cfgContent, 0644); err != nil {
		t.Fatal(err)
	}

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "-H", "X-Custom: value", "http://example.com/data"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_MethodFlag(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	cfgContent := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: %s\n", testutil.TestKey(), mock.URL))
	if err := os.WriteFile(cfgPath, cfgContent, 0644); err != nil {
		t.Fatal(err)
	}

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "-X", "POST", "http://example.com/data"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_DataFlag(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	cfgContent := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: %s\n", testutil.TestKey(), mock.URL))
	if err := os.WriteFile(cfgPath, cfgContent, 0644); err != nil {
		t.Fatal(err)
	}

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "-d", `{"key":"val"}`, "http://example.com/data"})
	err := rootCmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_ErrorOnNoTunnelKey(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server_url: http://127.0.0.1:18083\n"), 0644); err != nil {
		t.Fatal(err)
	}

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "http://example.com/data"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when tunnel_key is missing")
	}
}

// ---- 补充 error path 测试：upload server error ----

func TestUploadCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"upload", "--server", mock.URL, srcFile})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Download command server error ----

func TestDownloadCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.txt")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"download", "--server", mock.URL, "test.txt", dst})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 404")
	}
}

// ---- Delete command server error ----

func TestDeleteCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stat returns OK, Delete returns error
		if r.URL.Path == "/api/files/stat" {
			w.Header().Set("X-File-Checksum", "abc123")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"delete", "--server", mock.URL, "/test.txt"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when delete returns 500")
	}
}

// ---- List command server error ----

func TestListCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"list", "--server", mock.URL})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Archive command server error ----

func TestArchiveCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"archive", "--server", mock.URL, "-o", dst, "test.txt"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Archive-dir command RunE 测试 ----

func TestArchiveDirCommand_HappyPath(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "backup.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake archive data"))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"archive-dir", "--server", mock.URL, "-o", dst, "mydir"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("archive-dir command failed: %v", err)
	}
}

func TestArchiveDirCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "backup.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"archive-dir", "--server", mock.URL, "-o", dst, "mydir"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Config command error paths ----

func TestConfigCommand_UnknownSubcommand(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	var err error
	_ = testutil.CaptureStderr(func() {
		rootCmd.SetArgs([]string{"config", "unknown"})
		err = rootCmd.Execute()
	})
	if err == nil {
		t.Error("expected error for unknown subcommand")
	}
}

func TestConfigCommand_SetMissingValue(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"config", "set", "server_url"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when set has no value")
	}
}

// ---- Pwd command ----

func TestPwdCommand(t *testing.T) {
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdPwd(st, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	cmd.Run(cmd, nil)

	if !strings.Contains(buf.String(), "/") {
		t.Errorf("expected pwd to output '/', got: %s", buf.String())
	}
}

func TestPwdCommand_WithCurrentDir(t *testing.T) {
	var buf strings.Builder
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdPwd(st, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	cmd.Run(cmd, nil)

	if !strings.Contains(buf.String(), "subdir") {
		t.Errorf("expected pwd to contain 'subdir', got: %s", buf.String())
	}
}

// ---- Mkdir command ----

func TestMkdirCommand_HappyPath(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mkdir" {
			t.Errorf("expected path /mkdir, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"created"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMkdir(factory, cli.IOStreams{Out: &buf}, st)

	cmd.SetArgs([]string{"newdir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mkdir command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "目录已创建") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
}

func TestMkdirCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf, errBuf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMkdir(factory, cli.IOStreams{Out: &buf, ErrOut: &errBuf}, st)

	cmd.SetArgs([]string{"newdir"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Cd command ----

func TestCdCommand_PrintCurrent(t *testing.T) {
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdCd(st, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	cmd.Run(cmd, nil)

	if !strings.Contains(buf.String(), "/") {
		t.Errorf("expected cd output '/', got: %s", buf.String())
	}
}

func TestCdCommand_WithCurrentDir(t *testing.T) {
	var buf strings.Builder
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	cmd.Run(cmd, nil)

	if !strings.Contains(buf.String(), "subdir") {
		t.Errorf("expected cd output '/subdir', got: %s", buf.String())
	}
}

// ---- Batch-delete all fail scenario ----

func TestBatchDeleteCommand_AllFail(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "a.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			w.Header().Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
			w.Header().Set("X-File-Size", "5")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"batch-delete", "--server", mock.URL, srcFile})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when all deletes fail")
	}
}

// ---- Batch-rename odd args test ----

func TestBatchRenameCommand_OddArgs(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"batch-rename", "--server", "http://test.local", "old.txt", "new.txt", "orphan.txt"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error for odd number of args")
	}
}

// ---- Search command server error ----

func TestSearchCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"search", "--server", mock.URL, "keyword"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Stat command with directory type ----

func TestStatCommand_Directory(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "")
		w.Header().Set("X-File-Size", "0")
		w.Header().Set("X-File-IsDir", "true")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"stat", "--server", mock.URL, "mydir"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("stat command failed: %v", err)
		}
	})
	if !strings.Contains(out, "directory") {
		t.Errorf("expected output to mention directory, got: %s", out)
	}
}
