// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// -- helpers for tests --

// setupViperForSighup 配置全局 viper 使其能通过 ReadInConfig 读取指定配置文件。
func setupViperForSighup(cfgPath string) {
	v := viper.GetViper()
	v.SetConfigFile(cfgPath)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("SPROXY")
	v.AutomaticEnv()
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
	setupViperForSighup(cfgPath)

	// Set up the cfgFile global
	cfgFile = cfgPath
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
	setupViperForSighup(cfgPath)

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
	setupViperForSighup(cfgPath)

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

	_ = captureStderr(func() {
		handleSighup(initialCfg, nil)
	})
}

func TestRunServer_ListenAndServeError(t *testing.T) {
	// 先占用一个端口
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

	v := viper.GetViper()
	cfgFile = filepath.Join(tmpDir, "sproxy.yaml")
	v.SetConfigFile(cfgFile)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("SPROXY")
	v.AutomaticEnv()
	_ = v.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	_ = v.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	_ = v.BindPFlag("tunnel_key", cmd.Flags().Lookup("tunnel-key"))
	v.Set("addr", occupiedAddr)
	v.Set("uploads_dir", tmpDir)
	v.Set("tunnel_key", "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	v.Set("log_level", "error")
	t.Cleanup(func() {
		cfgPtr.Store(nil)
	})

	err = runServer(cmd, nil)
	if err == nil {
		t.Fatal("expected error when port is occupied")
	}
}

// captureStdout 捕获 stdout 输出的辅助函数（与 captureStderr 对称，用于 initLogger 测试）。
func captureStdout(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
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

				output := captureStdout(func() {
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
	oldCfgFile := cfgFile
	t.Cleanup(func() { cfgFile = oldCfgFile })

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
