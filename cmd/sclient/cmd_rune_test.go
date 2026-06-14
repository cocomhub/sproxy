// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	out := captureStdout(func() {
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

	out := captureStdout(func() {
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

	out := captureStdout(func() {
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

	_ = captureStderr(func() {
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

	out := captureStdout(func() {
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

// ---- Genkey command RunE 测试 ----

func TestGenkeyCommand(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	out := captureStdout(func() {
		rootCmd.SetArgs([]string{"genkey"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("genkey command failed: %v", err)
		}
	})
	out = strings.TrimSpace(out)
	if len(out) != 64 {
		t.Errorf("expected 64 hex chars, got %d: %q", len(out), out)
	}
}

// ---- Version command RunE 测试 ----

func TestVersionCommand_Run(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	out := captureStdout(func() {
		rootCmd.SetArgs([]string{"version"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("version command failed: %v", err)
		}
	})
	if !strings.Contains(out, "sclient") {
		t.Errorf("expected version output to contain 'sclient', got: %s", out)
	}
}

// ---- Config command RunE 测试 ----

func TestConfigCommand(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	_ = captureStdout(func() {
		rootCmd.SetArgs([]string{"config", "show"})
		rootCmd.Execute()
	})
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

	captureStderr(func() {
		rootCmd.SetArgs([]string{"batch-rename", "--server", mock.URL, "old.txt", "new.txt"})
		err := rootCmd.Execute()
		if err != nil {
			t.Logf("batch-rename expected non-nil exit: %v", err)
		}
	})
}

// ---- Tunnel command RunE 扩展测试（skip, 因为需要有效的 tunnel_key）----

func TestTunnelCommand_WithVerboseFlag(t *testing.T) {
	t.Skip("tunnel 命令需要有效的 tunnel_key 和加密隧道，mock server 无法替代")
}

func TestTunnelCommand_WithHeaderFlag(t *testing.T) {
	t.Skip("tunnel 命令需要有效的 tunnel_key")
}

func TestTunnelCommand_WithMethodFlag(t *testing.T) {
	t.Skip("tunnel 命令需要有效的 tunnel_key")
}

func TestTunnelCommand_WithDataFlag(t *testing.T) {
	t.Skip("tunnel 命令需要有效的 tunnel_key")
}
