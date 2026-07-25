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
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSearch(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"report"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("search command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "report.pdf") {
		t.Errorf("expected output to contain report.pdf, got: %s", buf.String())
	}
}

func TestSearchCommand_NoResults(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[],"total":0}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSearch(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"nonexistent"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("search command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "no files found") {
		t.Errorf("expected 'no files found' message, got: %s", buf.String())
	}
}

func TestSearchCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSearch(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"keyword"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"old.txt", "new.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mv command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已重命名") {
		t.Errorf("expected rename success message, got: %s", buf.String())
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdMv(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"old.txt", "new.txt"})
	_ = cmd.Execute()
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdStat(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"test.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stat command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "abc123def456") {
		t.Errorf("expected checksum in output, got: %s", buf.String())
	}
}

func TestStatCommand_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdStat(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"test.txt"})
	_ = cmd.Execute()
}

func TestStatCommand_Directory(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "")
		w.Header().Set("X-File-Size", "0")
		w.Header().Set("X-File-IsDir", "true")
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdStat(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"mydir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stat command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "directory") {
		t.Errorf("expected output to mention directory, got: %s", buf.String())
	}
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdBatchDelete(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{srcFile})
	_ = cmd.Execute()
}

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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdBatchDelete(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{srcFile})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when all deletes fail")
	}
}

// ---- Archive command RunE 测试 ----

func TestArchiveCommand(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake archive data"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdArchive(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"-o", dst, "test.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive command failed: %v", err)
	}
}

func TestArchiveCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdArchive(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"-o", dst, "test.txt"})
	err := cmd.Execute()
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdArchiveDir(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"-o", dst, "mydir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive-dir command failed: %v", err)
	}
}

func TestArchiveDirCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "backup.tar.gz")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdArchiveDir(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"-o", dst, "mydir"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Genkey 测试已迁移到 genkey_test.go ----
// TestGenkeyCommand 已移除，使用 TestNewCmdGenkey 替代

// ---- Version 测试已迁移到新工厂函数 ----
// TestVersionCommand_Run 已移除，版本命令不再通过 rootCmd 注册

// ---- Version 子命令 error 测试（已迁移）----

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
	svc := client.NewFileClient("http://127.0.0.1:18083")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard})
	cmd.SetArgs([]string{"http://example.com"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when tunnel_key is missing")
	}
}

func TestTunnelCommand_WithConfigKey(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":200}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL, client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: &buf, Out: &buf})
	cmd.SetArgs([]string{"http://any-host.local/data"})
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error after config: %v", err)
	}
}

func TestTunnelCommand_HeaderFlag(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL, client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: &buf, Out: &buf})
	cmd.SetArgs([]string{"-H", "X-Custom: value", "http://example.com/data"})
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_MethodFlag(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL, client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: &buf, Out: &buf})
	cmd.SetArgs([]string{"-X", "POST", "http://example.com/data"})
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_DataFlag(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL, client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: &buf, Out: &buf})
	cmd.SetArgs([]string{"-d", `{"key":"val"}`, "http://example.com/data"})
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error: %v", err)
	}
}

func TestTunnelCommand_ErrorOnNoTunnelKey(t *testing.T) {
	svc := client.NewFileClient("http://127.0.0.1:18083")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard})
	cmd.SetArgs([]string{"http://example.com/data"})
	err := cmd.Execute()
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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdBatchRename(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"old1.txt", "new1.txt", "old2.txt", "new2.txt"})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("batch-rename command failed: %v", err)
	}
}

func TestBatchRenameCommand_StatFails(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdBatchRename(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"old.txt", "new.txt"})
	err := cmd.Execute()
	if err != nil {
		t.Logf("batch-rename expected non-nil exit: %v", err)
	}
}

func TestBatchRenameCommand_OddArgs(t *testing.T) {
	cmd := NewCmdBatchRename(clientfactory.NewMock(nil, nil), cli.IOStreams{ErrOut: io.Discard})
	cmd.SetArgs([]string{"old.txt", "new.txt", "orphan.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for odd number of args")
	}
}

// ---- 补充 error path 测试 ----

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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{srcFile})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

func TestDownloadCommand_ServerError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.txt")

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"test.txt", dst})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 404")
	}
}

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

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdDelete(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs([]string{"/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when delete returns 500")
	}
}

func TestListCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdList(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, st)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

// ---- Config command error paths ----

func TestConfigCommand_UnknownSubcommand(t *testing.T) {
	oldCfgProvider := cfgProvider
	cfgProvider = sclientcfg.New("")
	t.Cleanup(func() { cfgProvider = oldCfgProvider })

	var buf strings.Builder
	cmd := NewCmdConfig(nil, cli.IOStreams{ErrOut: &buf}, new(string))
	cmd.SetArgs([]string{"unknown"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for unknown subcommand")
	}
}

func TestConfigCommand_SetMissingValue(t *testing.T) {
	oldCfgProvider := cfgProvider
	cfgProvider = sclientcfg.New("")
	t.Cleanup(func() { cfgProvider = oldCfgProvider })

	var buf strings.Builder
	cmd := NewCmdConfig(nil, cli.IOStreams{ErrOut: &buf}, new(string))
	cmd.SetArgs([]string{"set", "server_url"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when set has no value")
	}
}

// ---- Pwd command（已迁移）----

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

// ---- Mkdir command（已迁移）----

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

// ---- Cd command（已迁移）----

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

func TestCdCommand_ChangeToSubdir(t *testing.T) {
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"newdir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "newdir" {
		t.Fatalf("expected currentDir 'newdir', got %q", st.CurrentDir)
	}
}

func TestCdCommand_ChangeToRoot(t *testing.T) {
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"/"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "" {
		t.Fatalf("expected currentDir '', got %q", st.CurrentDir)
	}
}

func TestCdCommand_ChangeToParent(t *testing.T) {
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{".."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "" {
		t.Fatalf("expected currentDir '', got %q", st.CurrentDir)
	}
}

func TestCdCommand_ChangeToParentFromNested(t *testing.T) {
	st := &state.State{CurrentDir: "a/b/c"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{".."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "a/b" {
		t.Fatalf("expected currentDir 'a/b', got %q", st.CurrentDir)
	}
}

func TestCdCommand_ChangeToDot(t *testing.T) {
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "subdir" {
		t.Fatalf("expected currentDir 'subdir', got %q", st.CurrentDir)
	}
}

func TestCdCommand_InvalidPath(t *testing.T) {
	var errBuf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: &errBuf})
	cmd.SetArgs([]string{"../outside"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if !strings.Contains(errBuf.String(), "无效的路径") {
		t.Errorf("expected error message '无效的路径', got %q", errBuf.String())
	}
	if st.CurrentDir != "" {
		t.Fatalf("expected currentDir to remain unchanged '', got %q", st.CurrentDir)
	}
}

func TestCdCommand_FromRootToParent(t *testing.T) {
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{".."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "" {
		t.Fatalf("expected currentDir '', got %q", st.CurrentDir)
	}
}

func TestCdCommand_ChangeToNested(t *testing.T) {
	st := &state.State{CurrentDir: "subdir"}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"nested"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "subdir/nested" {
		t.Fatalf("expected currentDir 'subdir/nested', got %q", st.CurrentDir)
	}
}

func TestCdCommand_CleanDots(t *testing.T) {
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdCd(st, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"././subdir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cd command failed: %v", err)
	}
	if st.CurrentDir != "subdir" {
		t.Fatalf("expected currentDir 'subdir', got %q", st.CurrentDir)
	}
}

// ---- resolveOutputPath 测试 ----

func TestResolveOutputPath_SpecifiedFile(t *testing.T) {
	oldDir := currentDir
	currentDir = t.TempDir()
	t.Cleanup(func() { currentDir = oldDir })

	got, err := resolveOutputPath("http://example.com/data/file.txt", "/tmp/out.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tmp/out.txt" {
		t.Errorf("expected /tmp/out.txt, got %s", got)
	}
}

func TestResolveOutputPath_FromURLPath(t *testing.T) {
	oldDir := currentDir
	currentDir = t.TempDir()
	t.Cleanup(func() { currentDir = oldDir })

	got, err := resolveOutputPath("http://example.com/data/report.pdf", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Join(currentDir, "report.pdf") {
		t.Errorf("expected %s, got %s", filepath.Join(currentDir, "report.pdf"), got)
	}
}

func TestResolveOutputPath_RootPathDefaultsToIndexHTML(t *testing.T) {
	oldDir := currentDir
	currentDir = t.TempDir()
	t.Cleanup(func() { currentDir = oldDir })

	got, err := resolveOutputPath("http://example.com/", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Join(currentDir, "index.html") {
		t.Errorf("expected %s, got %s", filepath.Join(currentDir, "index.html"), got)
	}
}

func TestResolveOutputPath_InvalidURL(t *testing.T) {
	oldDir := currentDir
	currentDir = t.TempDir()
	t.Cleanup(func() { currentDir = oldDir })

	_, err := resolveOutputPath("://invalid-url\t", "")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestResolveOutputPath_ConflictAppendsSuffix(t *testing.T) {
	oldDir := currentDir
	currentDir = t.TempDir()
	t.Cleanup(func() { currentDir = oldDir })

	// Create a file that conflicts
	conflict := filepath.Join(currentDir, "data.txt")
	if err := os.WriteFile(conflict, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveOutputPath("http://example.com/data.txt", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := filepath.Join(currentDir, "data.txt.1")
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}
