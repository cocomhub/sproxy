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

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
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

// ---- resolveTunnelKey 边界测试（已废除）----
func TestRunServer_VersionFlag(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	_ = cmd.Flags().Set("version", "true")

	stdout := testutil.CaptureStdout(func() {
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

	// 清除全局状态，避免之前测试的残留
	cfgPtr.Store(nil)
	cfgProvider = nil

	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	cmd := &cobra.Command{}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")
	setupRunServerAuthConfig(t, cmd)

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	// 轮询等待 cfgPtr 被初始化（server 启动成功），然后用实际地址连接
	waitForConfig(t, 5*time.Second)
	addr := cfgPtr.Load().Addr
	waitForServerReady(t, addr, 5*time.Second)
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

	// 清除全局状态，避免之前测试的残留
	cfgPtr.Store(nil)
	cfgProvider = nil

	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	before := runtime.NumGoroutine()
	cmd := &cobra.Command{}
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")
	setupRunServerAuthConfig(t, cmd)

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	// 轮询等待 cfgPtr 被初始化（server 启动成功），然后用实际地址连接
	waitForConfig(t, 5*time.Second)
	addr := cfgPtr.Load().Addr
	waitForServerReady(t, addr, 5*time.Second)
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

// ---- 构建配置（无 tunnel_key 引用）测试 ----

func TestBuildServerConfig_NoTLSFlag(t *testing.T) {
	tmpDir := t.TempDir()
	oldCfgFile := cfgFile
	t.Cleanup(func() { cfgFile = oldCfgFile })
	cfgFile = filepath.Join(tmpDir, "nonexistent.yaml")

	cmd := &cobra.Command{}
	cmd.Flags().Bool("no-tls", false, "")
	cmd.Flags().String("addr", ":18083", "")
	cmd.Flags().String("uploads-dir", tmpDir, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	t.Cleanup(func() { cfgProvider = nil })

	cfg, err := buildServerConfig(cmd)
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	if cfg.TLS.Enabled {
		t.Error("expected TLS.Enabled to be false when --no-tls is set")
	}
}

func TestBuildServerConfig_NoTLSFlagDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	oldCfgFile := cfgFile
	t.Cleanup(func() { cfgFile = oldCfgFile })
	cfgFile = filepath.Join(tmpDir, "nonexistent.yaml")

	cmd := &cobra.Command{}
	cmd.Flags().Bool("no-tls", false, "")
	cmd.Flags().String("addr", ":18083", "")
	cmd.Flags().String("uploads-dir", tmpDir, "")
	// 不设置 --no-tls，验证 TLS 默认启用

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	t.Cleanup(func() { cfgProvider = nil })

	cfg, err := buildServerConfig(cmd)
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	if !cfg.TLS.Enabled {
		t.Error("expected TLS.Enabled to be true by default")
	}
}

// ---- 认证 fail-fast 测试 ----

// TestRunServer_RejectsStartupWithoutAuth 验证 fail-fast：无 access_keys 且 api_keys
// 未启用时，runServer 直接返回错误拒绝启动（认证驱动隧道要求）。
func TestRunServer_RejectsStartupWithoutAuth(t *testing.T) {
	cfgPtr.Store(nil)
	cfgProvider = nil

	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cmd := &cobra.Command{}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("uploads-dir", t.TempDir(), "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	t.Cleanup(func() { cfgProvider = nil })

	err := runServer(cmd, nil)
	if err == nil {
		t.Fatal("runServer without access_keys should fail fast")
	}
	if !strings.Contains(err.Error(), "拒绝启动") {
		t.Errorf("expected fail-fast error containing '拒绝启动', got: %v", err)
	}
}

// setupRunServerAuthConfig 为 runServer 测试配置 access_keys（认证驱动启动必需）
// 与 uploads_dir，使服务器能通过 fail-fast 检查正常启动。
func setupRunServerAuthConfig(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cmd.Flags().String("uploads-dir", t.TempDir(), "")
	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	cfgProvider.Set("access_keys", []map[string]any{{"key": "sk-test-0000000000000000", "secret": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "mesh_id": "test"}})
	t.Cleanup(func() { cfgProvider = nil })
}

// ---- 测试辅助函数 ----

// waitForServerReady 轮询等待 HTTP 服务器就绪（通过 HTTP 连接，支持 TLS 和 HTTP）。
func waitForServerReady(t testing.TB, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not become ready within %v (addr=%s)", timeout, addr)
}

// waitForConfig 轮询等待 cfgPtr 被 server 初始化。
func waitForConfig(t testing.TB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cfg := cfgPtr.Load(); cfg != nil && cfg.Addr != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("config did not become ready within %v", timeout)
}
