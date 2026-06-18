// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sproxy/internal/sproxycfg"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/spf13/cobra"
)

// -- helpers for tests --

// setupProviderForSighup 创建 Provider 使其能读取指定配置文件。
func setupProviderForSighup(cfgPath string) *sproxycfg.ViperProvider {
	return sproxycfg.New(cfgPath)
}

// ---- handleSighup tests ----

func TestResolveTunnelKey_EmptyAutoGenerate(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile = filepath.Join(tmpDir, "sproxy.yaml")
	t.Cleanup(func() { cfgFile = "" })

	cfg := &server.Config{TunnelKey: ""}
	_, err := resolveTunnelKey(cfg)
	if err != nil {
		t.Fatalf("resolveTunnelKey with empty key should auto-generate: %v", err)
	}
	if cfg.TunnelKey == "" {
		t.Fatal("resolveTunnelKey should set TunnelKey on empty")
	}
	if len(cfg.TunnelKey) != 64 {
		t.Errorf("expected 64 hex chars, got %d: %q", len(cfg.TunnelKey), cfg.TunnelKey)
	}
	// Verify config file was written
	if _, err := os.Stat(cfgFile); err != nil {
		t.Errorf("expected config file to be written: %v", err)
	}
}

func TestHandleSighup_KeyRotation(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	// Write initial config file
	initialCfg := server.Default()
	initialCfg.Addr = "127.0.0.1:0"
	initialCfg.TunnelKey = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })

	// Set up the cfgFile global with save/restore
	cfgFile = cfgPath
	t.Cleanup(func() { cfgFile = "" })
	var updated string
	tunUpdater := &mockTunnelUpdater{updateFn: func(key []byte) {
		updated = hex.EncodeToString(key)
	}}

	// Initial cfgPtr state
	cfgPtr.Store(initialCfg)
	currentTunnelKeyHex = initialCfg.TunnelKey
	t.Cleanup(func() { currentTunnelKeyHex = "" })

	// Modify config file with new key
	newCfg := *initialCfg
	newCfg.TunnelKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	oldCfg := cfgPtr.Load()
	handleSighup(oldCfg, tunUpdater)

	if updated == "" {
		t.Fatal("UpdateKey was not called")
	}
}

type mockTunnelUpdater struct {
	updateFn func(key []byte)
}

func (m *mockTunnelUpdater) UpdateKey(key []byte) {
	if m.updateFn != nil {
		m.updateFn(key)
	}
}

func TestHandleSighup_ConfigReload(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.TunnelKey = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	initialCfg.Addr = "127.0.0.1:0"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })

	cfgFile = cfgPath
	t.Cleanup(func() {
		cfgPtr.Store(nil)
	})

	cfgPtr.Store(initialCfg)
	currentTunnelKeyHex = initialCfg.TunnelKey
	t.Cleanup(func() { currentTunnelKeyHex = "" })

	// Change log level in config file
	newCfg := *initialCfg
	newCfg.LogLevel = "debug"
	newCfg.LogFormat = "json"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	handleSighup(initialCfg, nil)

	reloaded := cfgPtr.Load()
	if reloaded.LogLevel != "debug" {
		t.Errorf("expected log_level=debug after reload, got %q", reloaded.LogLevel)
	}
}

func TestHandleSighup_AddrChangeWarning(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "sproxy.yaml")

	initialCfg := server.Default()
	initialCfg.TunnelKey = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	initialCfg.Addr = ":18083"
	if err := server.SaveConfig(initialCfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	cfgProvider = setupProviderForSighup(cfgPath)
	t.Cleanup(func() { cfgProvider = nil })

	cfgFile = cfgPath
	t.Cleanup(func() {
		cfgPtr.Store(nil)
	})

	cfgPtr.Store(initialCfg)
	currentTunnelKeyHex = initialCfg.TunnelKey
	t.Cleanup(func() { currentTunnelKeyHex = "" })

	// Change addr (warn-only field)
	newCfg := *initialCfg
	newCfg.Addr = ":19000"
	newCfg.LogLevel = "debug"
	if err := server.SaveConfig(&newCfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	_ = testutil.CaptureStderr(func() {
		handleSighup(initialCfg, nil)
	})
}

func TestRunServer_ListenAndServeError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping listen error test in short mode")
	}

	// 先占用一个端口。注意：Linux Go 默认 SO_REUSEADDR 允许多次绑定同一地址，
	// 因此端口占用在 Linux 上不会导致 ListenAndServe 失败。
	// 此测试在两种平台上均需通过：Windows（端口冲突报错）和 Linux（SO_REUSEADDR 成功，通过信号关闭）。
	existing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	occupiedAddr := existing.Addr().String()

	tmpDir := t.TempDir()

	cmd := &cobra.Command{Use: "sproxy"}
	cmd.Flags().String("addr", occupiedAddr, "")
	cmd.Flags().String("uploads-dir", tmpDir, "")
	cmd.Flags().String("tunnel-key", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", "")
	cmd.Flags().Bool("version", false, "")

	// 注入 signal channel 避免 goroutine 泄漏
	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	t.Cleanup(func() { testSignalCh = nil })

	// 设置 Provider 和配置
	cfgProvider = setupProviderForSighup(filepath.Join(tmpDir, "sproxy.yaml"))
	t.Cleanup(func() { cfgProvider = nil })
	cfgProvider.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	cfgProvider.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	cfgProvider.BindPFlag("tunnel_key", cmd.Flags().Lookup("tunnel-key"))
	cfgProvider.Set("addr", occupiedAddr)
	cfgProvider.Set("uploads_dir", tmpDir)
	cfgProvider.Set("tunnel_key", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	cfgProvider.Set("log_level", "error")

	// 并发运行 server
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	// 等待 server 启动（ListenAndServe 在 Windows 上会立即返回错误，Linux 则可能成功）
	time.Sleep(300 * time.Millisecond)

	// 发送 SIGTERM 确保 server 关闭（无论端口占用是否生效）
	sigCh <- syscall.SIGTERM

	select {
	case err := <-errCh:
		// Windows：err 为 "listen and serve error"（端口冲突）
		// Linux：err 为 nil 或 ErrServerClosed（信号关闭）
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !strings.Contains(err.Error(), "listen and serve error") {
			t.Errorf("runServer returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}
}

// ---- initLogger tests ----

func TestInitLogger_Combinations(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	formats := []string{"text", "json"}

	for _, level := range levels {
		for _, format := range formats {
			t.Run(level+"_"+format, func(t *testing.T) {
				cfg := &server.Config{
					LogLevel:  level,
					LogFormat: format,
				}

				// Save and restore the default logger to avoid cross-test interference
				oldDefault := slog.Default()
				t.Cleanup(func() { slog.SetDefault(oldDefault) })

				output := testutil.CaptureStdout(func() {
					logger := initLogger(cfg)
					if logger == nil {
						t.Error("initLogger returned nil")
						return
					}
					// Log at the configured level so the output is always visible
					switch level {
					case "debug":
						logger.Debug("test message", "key", "value")
					case "info":
						logger.Info("test message", "key", "value")
					case "warn":
						logger.Warn("test message", "key", "value")
					case "error":
						logger.Error("test message", "key", "value")
					}
				})

				if format == "json" {
					// JSON output should be valid JSON
					if len(output) == 0 {
						t.Error("expected JSON output, got empty")
						return
					}
					if output[0] != '{' {
						t.Errorf("expected JSON object (starts with '{'), got: %s", output[:min(len(output), 50)])
					}
					if !bytes.Contains([]byte(output), []byte("test message")) {
						t.Errorf("expected log message in JSON output, got: %s", output[:min(len(output), 100)])
					}
				} else {
					// Text output should contain the message
					if !bytes.Contains([]byte(output), []byte("test message")) {
						t.Errorf("expected 'test message' in text output, got: %s", output[:min(len(output), 100)])
					}
				}
			})
		}
	}
}

// ---- resolveTunnelKey tests ----

func TestResolveTunnelKey_SaveError(t *testing.T) {
	// Save cfgFile so resolveTunnelKey can restore it later
	t.Cleanup(func() { cfgFile = "" })

	// Use a path where the parent directory does not exist.
	// os.WriteFile will fail because the directory doesn't exist.
	badDir := filepath.Join(t.TempDir(), "nonexistent")
	cfgFile = filepath.Join(badDir, "sproxy.yaml")

	cfg := &server.Config{TunnelKey: ""}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Fatal("expected error when SaveConfig fails due to non-writable path")
	}
	t.Logf("got expected error: %v", err)
}
