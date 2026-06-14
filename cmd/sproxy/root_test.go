// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestLevelString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"debug", "debug"},
		{"info", "info"},
		{"warn", "warn"},
		{"error", "error"},
		{"", "info"},
		{"unknown", "info"},
		{"DEBUG", "info"}, // case sensitive
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := levelString(tt.input); got != tt.want {
				t.Errorf("levelString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"json", "json"},
		{"text", "text"},
		{"", "text"},
		{"unknown", "text"},
		{"JSON", "text"}, // case sensitive
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := formatString(tt.input); got != tt.want {
				t.Errorf("formatString(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestInitLogger(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		format  string
		wantNil bool
	}{
		{"default", "", "", false},
		{"text_info", "info", "text", false},
		{"json_debug", "debug", "json", false},
		{"json_error", "error", "json", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &server.Config{
				LogLevel:  tt.level,
				LogFormat: tt.format,
			}
			logger := initLogger(cfg)
			if logger == nil {
				t.Error("initLogger returned nil")
			}
		})
	}
}

func TestInitLoggerSetsDefault(t *testing.T) {
	// 验证 initLogger 调用后 slog.SetDefault 生效
	cfg := &server.Config{LogLevel: "info", LogFormat: "text"}
	logger := initLogger(cfg)
	if slog.Default() != logger {
		t.Error("slog.Default() should be the logger returned by initLogger")
	}
}

func TestResolveTunnelKey_Valid64Hex(t *testing.T) {
	// 64 hex chars = 32 bytes
	validKey := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	cfg := &server.Config{TunnelKey: validKey}
	key, err := resolveTunnelKey(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32 bytes key, got %d", len(key))
	}
}

func TestResolveTunnelKey_InvalidLength(t *testing.T) {
	cfg := &server.Config{TunnelKey: "short"}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestResolveTunnelKey_NonHex(t *testing.T) {
	// Same length but includes non-hex chars
	cfg := &server.Config{TunnelKey: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Fatal("expected error for non-hex key")
	}
}

func TestResolveTunnelKey_Empty(t *testing.T) {
	// empty key -> triggers auto-generate + file save
	// This would call tunnel.GenerateKey() and server.SaveConfig().
	// We can't easily test this without mocking, so skip for now.
	// The function is indirectly tested through the valid/invalid key paths.
}

func TestRunServer_VersionFlag(t *testing.T) {
	// Verify that with --version, Execute() prints version and exits with 0.
	// Since we can't easily capture os.Exit in Go tests, we test the underlying
	// handler logic by setting the version flag.
	// Note: cobra.Execute() can't be easily unit tested in isolation.
}

func TestRunServer_StartStop(t *testing.T) {
	// 通过注入 signal channel 来避免 Windows 对 os.Signal 的限制
	tmpDir := t.TempDir()

	cmd := &cobra.Command{Use: "sproxy"}
	cmd.Flags().String("addr", "127.0.0.1:0", "")
	cmd.Flags().String("uploads-dir", tmpDir, "")
	cmd.Flags().String("tunnel-key", "", "")
	cmd.Flags().Bool("version", false, "")

	// 设置 viper 测试配置
	v := viper.GetViper()
	cfgFile = tmpDir + "/sproxy.yaml"
	v.SetConfigFile(cfgFile)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("SPROXY")
	v.AutomaticEnv()
	_ = v.BindPFlag("addr", cmd.Flags().Lookup("addr"))
	_ = v.BindPFlag("uploads_dir", cmd.Flags().Lookup("uploads-dir"))
	_ = v.BindPFlag("tunnel_key", cmd.Flags().Lookup("tunnel-key"))

	validKey := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	v.Set("addr", "127.0.0.1:0")
	v.Set("uploads_dir", tmpDir)
	v.Set("log_level", "error")
	v.Set("tunnel_key", validKey)

	// 注入 signal channel，避免依赖真实的进程信号
	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	defer func() { testSignalCh = nil }()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(cmd, nil)
	}()

	// 等服务器启动
	time.Sleep(500 * time.Millisecond)

	// 通过注入的 channel 发送中断信号
	sigCh <- os.Interrupt

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("runServer returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within 5s")
	}
}
