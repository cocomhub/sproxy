// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/spf13/cobra"
)

// ---- levelString 边界测试 ----

func TestLevelString_AllCases(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"debug", "debug"},
		{"info", "info"},
		{"warn", "warn"},
		{"error", "error"},
		{"unknown", "info"},
		{"", "info"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := levelString(tt.input); got != tt.expected {
				t.Errorf("levelString(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ---- formatString 边界测试 ----

func TestFormatString_AllCases(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"json", "json"},
		{"text", "text"},
		{"unknown", "text"},
		{"", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := formatString(tt.input); got != tt.expected {
				t.Errorf("formatString(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ---- initLogger 边界测试 ----
// TestInitLogger_Boundaries 只覆盖边界值（default/unknown），不覆盖 root_extra_test.go 中已有的正常组合测试

func TestInitLogger_Boundaries(t *testing.T) {
	tests := []struct {
		name             string
		logLevel, logFmt string
	}{
		{"default-level_default-format", "", ""},
		{"unknown-level", "unknown", "text"},
		{"unknown-format", "info", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := initLogger(&server.Config{LogLevel: tt.logLevel, LogFormat: tt.logFmt})
			if logger == nil {
				t.Fatal("expected non-nil logger")
			}
		})
	}
}

// ---- resolveTunnelKey 边界测试 ----

func TestResolveTunnelKey_Valid(t *testing.T) {
	validKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg := &server.Config{TunnelKey: validKey}
	key, err := resolveTunnelKey(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("expected 32 bytes, got %d", len(key))
	}
}

func TestResolveTunnelKey_InvalidLength(t *testing.T) {
	cfg := &server.Config{TunnelKey: "short"}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Error("expected error for short tunnel key")
	}
}

func TestResolveTunnelKey_NonHex(t *testing.T) {
	cfg := &server.Config{TunnelKey: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Error("expected error for non-hex tunnel key")
	}
}

// TestResolveTunnelKey_EmptyAutoGenFail 测试空密钥 + cfgFile 不可写时的错误路径
// 与 root_extra_test.go 中 TestResolveTunnelKey_SaveError 类似但使用空路径场景
func TestResolveTunnelKey_EmptyAutoGenFail(t *testing.T) {
	// 保存并恢复全局 cfgFile
	oldCfgFile := cfgFile
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cfgFile = filepath.Join(t.TempDir(), "nonexistent", "sproxy.yaml")
	cfg := &server.Config{TunnelKey: ""}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Fatal("expected error when auto-generate fails due to non-writable path")
	}
	t.Logf("got expected error: %v", err)
}

// ---- runServer 边界测试 ----
// TestRunServer_VersionFlag 确保 --version 正确输出

func TestRunServer_VersionFlag(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	_ = cmd.Flags().Set("version", "true")

	stdout := captureOutput(func() {
		_ = runServer(cmd, nil)
	})
	if !strings.Contains(stdout, "Version:") {
		t.Errorf("expected Version output, got: %s", stdout)
	}
}

// TestRunServer_SignalShutdown 验证 server 能通过 SIGTERM 正常关闭
func TestRunServer_SignalShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping signal shutdown test in short mode")
	}

	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	cmd := &cobra.Command{}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("version", false, "")

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	time.Sleep(300 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("runServer returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}

	// 确认没有明显的 goroutine 泄漏（允许少量增长）
	after := runtime.NumGoroutine()
	if after > runtime.GOMAXPROCS(0)*3+10 {
		t.Errorf("suspicious number of goroutines after shutdown: %d", after)
	}
}

func TestRunServer_SignalGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping goroutine leak test in short mode")
	}

	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	before := runtime.NumGoroutine()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("addr", "127.0.0.1:0", "")

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	time.Sleep(500 * time.Millisecond)
	sigCh <- syscall.SIGTERM

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("runServer returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}

	after := runtime.NumGoroutine()
	if after > before+5 {
		t.Errorf("possible goroutine leak after signal shutdown: before=%d, after=%d", before, after)
	}
}

// ---- 测试辅助函数 ----

func captureOutput(fn func()) string {
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	_ = w.Close()

	var buf strings.Builder
	var dst [4096]byte
	for {
		n, err := r.Read(dst[:])
		if n > 0 {
			buf.Write(dst[:n])
		}
		if err != nil {
			break
		}
	}
	return buf.String()
}
