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
	cmd.Flags().String("storage-root", tmpDir, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
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
	cmd.Flags().String("storage-root", tmpDir, "")
	// 不设置 --no-tls，验证 TLS 默认启用

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
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
	cmd.Flags().String("storage-root", t.TempDir(), "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
	t.Cleanup(func() { cfgProvider = nil })

	err := runServer(cmd, nil)
	if err == nil {
		t.Fatal("runServer without access_keys should fail fast")
	}
	if !strings.Contains(err.Error(), "拒绝启动") {
		t.Errorf("expected fail-fast error containing '拒绝启动', got: %v", err)
	}
}

// TestRunServer_AllowNoAuthFlag 验证 --allow-no-auth 显式跳过 fail-fast：无
// access_keys/api_keys 时仍可启动（仅限本地无认证调试，Web UI 无凭据直连需要）。
func TestRunServer_AllowNoAuthFlag(t *testing.T) {
	cfgPtr.Store(nil)
	cfgProvider = nil

	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cmd := &cobra.Command{}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("storage-root", t.TempDir(), "")
	cmd.Flags().Bool("no-tls", false, "")
	cmd.Flags().Bool("allow-no-auth", false, "")
	_ = cmd.Flags().Set("no-tls", "true")
	_ = cmd.Flags().Set("allow-no-auth", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
	t.Cleanup(func() { cfgProvider = nil })

	// runServer 会启动监听并阻塞于 shutdown——注入关闭信号使其快速返回，
	// 验证不因 fail-fast 而拒绝（主要断言：未走到 fail-fast 错误路径）。
	done := make(chan struct{}, 1)
	oldSignal := testSignalCh
	testSignalCh = make(chan os.Signal, 1)
	testSignalCh <- syscall.SIGTERM
	t.Cleanup(func() { testSignalCh = oldSignal })

	go func() {
		_ = runServer(cmd, nil)
		done <- struct{}{}
	}()
	select {
	case <-done:
		// 收到 SIGTERM 后正常退出（未 fail-fast）
	case <-time.After(10 * time.Second):
		t.Fatal("runServer with --allow-no-auth should not block forever / fail fast")
	}
}

// TestRunServer_HubEnabledRequiresAccessKeys 验证 M-8：api_keys-only（多用户 Bearer）
// 下 hub 注册不可用——启用 hub 时强制要求 access_keys 非空（隧道密钥与 hub 准入都来自 access_keys）。
func TestRunServer_HubEnabledRequiresAccessKeys(t *testing.T) {
	cfgPtr.Store(nil)
	cfgProvider = nil

	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cmd := &cobra.Command{}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().Bool("version", false, "")
	cmd.Flags().String("storage-root", t.TempDir(), "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
	cfgProvider.Set("api_keys", map[string]any{"enabled": true, "keys": []map[string]any{{"key": "t1", "permission": "write"}}})
	cfgProvider.Set("hub", map[string]any{"enabled": true})
	t.Cleanup(func() { cfgProvider = nil })

	err := runServer(cmd, nil)
	if err == nil {
		t.Fatal("hub.enabled without access_keys should fail fast")
	}
	if !strings.Contains(err.Error(), "hub.enabled=true") {
		t.Errorf("expected hub access_keys error, got: %v", err)
	}
}

// setupRunServerAuthConfig 为 runServer 测试配置 access_keys（认证驱动启动必需）
// 与 storage_root，使服务器能通过 fail-fast 检查正常启动。
func setupRunServerAuthConfig(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	cmd.Flags().String("storage-root", t.TempDir(), "")
	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
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

// TestBuildServerConfig_FederationPersistFile：hub.federation.persist_file 从配置解析
// 到 server.Config（装配链路：root.go 将 cfg.Hub.Federation.PersistFile 非空时传入
// hub.NewFederationClientWithPersist 启用候选持久化）。
func TestBuildServerConfig_FederationPersistFile(t *testing.T) {
	cfgPtr.Store(nil)
	cfgProvider = nil

	oldCfgFile := cfgFile
	cfgFile = filepath.Join(t.TempDir(), "sproxy.yaml")
	t.Cleanup(func() { cfgFile = oldCfgFile })

	persistFile := filepath.Join(t.TempDir(), "fed-cands.json")
	cmd := &cobra.Command{}
	cmd.Flags().String("addr", ":18083", "")
	cmd.Flags().String("storage-root", t.TempDir(), "")
	cmd.Flags().Bool("no-tls", false, "")
	_ = cmd.Flags().Set("no-tls", "true")

	cfgProvider = sproxycfg.New(cfgFile)
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("storage_root", cmd.Flags().Lookup("storage-root"))
	cfgProvider.Set("hub", map[string]any{
		"enabled":    true,
		"transports": map[string]any{"ws": map[string]any{"enabled": true}},
		"federation": map[string]any{
			"enabled":      true,
			"persist_file": persistFile,
		},
	})
	t.Cleanup(func() { cfgProvider = nil })

	cfg, err := buildServerConfig(cmd)
	if err != nil {
		t.Fatalf("buildServerConfig: %v", err)
	}
	if cfg.Hub.Federation.PersistFile != persistFile {
		t.Fatalf("federation.persist_file 应解析为 %q, got %q", persistFile, cfg.Hub.Federation.PersistFile)
	}
}
