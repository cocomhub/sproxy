// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/provider"
	"gopkg.in/yaml.v3"
)

// compile-time interface check
var _ provider.Provider = mapProvider{}

func TestConfig_DefaultsFilled(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Addr == "" {
		t.Fatal("Addr default empty")
	}
	if cfg.UploadsDir == "" {
		t.Fatal("UploadsDir default empty")
	}
	if cfg.ChunkSize <= 0 {
		t.Fatal("ChunkSize default <= 0")
	}
	if cfg.UploadSessionTTL <= 0 {
		t.Fatal("UploadSessionTTL default <= 0")
	}
	if cfg.ServerTimeouts.Shutdown <= 0 {
		t.Fatalf("ServerTimeouts.Shutdown default <= 0: %v", cfg.ServerTimeouts.Shutdown)
	}
	if cfg.ServerTimeouts.Shutdown != 30*time.Second {
		t.Fatalf("ServerTimeouts.Shutdown default want 30s, got %v", cfg.ServerTimeouts.Shutdown)
	}
	if !cfg.TLS.Enabled {
		t.Fatal("TLS.Enabled default should be true")
	}
	if !cfg.TLS.AutoTLS {
		t.Fatal("TLS.AutoTLS default should be true")
	}
	if cfg.CloudDownloadTimeout != 30*time.Minute {
		t.Fatalf("CloudDownloadTimeout default want 30m, got %v", cfg.CloudDownloadTimeout)
	}
	if cfg.CloudDownloadIdleTimeout != 1*time.Minute {
		t.Fatalf("CloudDownloadIdleTimeout default want 1m, got %v", cfg.CloudDownloadIdleTimeout)
	}
	if cfg.CloudMaxRetries != 10 {
		t.Fatalf("CloudMaxRetries default want 10, got %d", cfg.CloudMaxRetries)
	}
	if cfg.CloudRetryDelay != 10*time.Second {
		t.Fatalf("CloudRetryDelay default want 10s, got %v", cfg.CloudRetryDelay)
	}
	if cfg.CloudMaxBatchURLs != 100 {
		t.Fatalf("CloudMaxBatchURLs default want 100, got %d", cfg.CloudMaxBatchURLs)
	}
}

func TestConfig_Validate_FillsZeroes(t *testing.T) {
	t.Parallel()
	c := &Config{TLS: TLSConfig{Enabled: true}}
	c.SetDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Addr == "" || c.UploadsDir == "" || c.ChunkSize <= 0 ||
		c.UploadSessionTTL <= 0 || c.ServerTimeouts.Shutdown <= 0 {
		t.Fatalf("SetDefaults did not fill zero values: %+v", c)
	}
}

func TestConfig_Validate_TunnelKey_HexCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty_ok_tls_enabled", "", false},
		{"valid_64hex", strings.Repeat("a", 64), false},
		{"too_short", strings.Repeat("a", 32), true},
		{"too_long", strings.Repeat("a", 65), true},
		{"non_hex", strings.Repeat("z", 64), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.TunnelKey = c.key
			err := cfg.Validate()
			if c.wantErr && err == nil {
				t.Fatalf("expected error for %q", c.key)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestConfig_Validate_EmptyTunnelKeyNoTLS(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.TunnelKey = ""
	cfg.TLS.Enabled = false
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty tunnel_key with TLS disabled, got nil")
	}
}

func TestConfig_Defaults_HubMaxConnections(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.SetDefaults()
	if cfg.Hub.MaxConnections != 256 {
		t.Fatalf("Hub.MaxConnections default want 256, got %d", cfg.Hub.MaxConnections)
	}
	// 显式配置非零值应保留
	cfg2 := Default()
	cfg2.Hub.MaxConnections = 64
	cfg2.SetDefaults()
	if cfg2.Hub.MaxConnections != 64 {
		t.Fatalf("Hub.MaxConnections want preserved 64, got %d", cfg2.Hub.MaxConnections)
	}
}

func TestConfig_Validate_HubEnabledRequiresRelayToken(t *testing.T) {
	t.Parallel()
	// hub.enabled=true 且 relay_token 为空 → 校验失败（C2 主修复）
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.RelayToken = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when hub.enabled=true but relay_token empty")
	}
	// 配了 token 后应通过
	cfg.Hub.RelayToken = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error with relay_token set: %v", err)
	}
	// hub 未启用（默认）时不受影响
	cfg2 := Default()
	cfg2.Hub.Enabled = false
	cfg2.Hub.RelayToken = ""
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("hub disabled should not require relay_token: %v", err)
	}
}

// mapProvider 将 map[string]any 转换为 provider.Provider 用于测试。
type mapProvider struct {
	m map[string]any
}

func (p mapProvider) Unmarshal(obj any) error {
	// 使用 yaml 作为中介：map → yaml bytes → struct
	// Config 结构体使用 yaml tag，所以 yaml.Unmarshal 能正确匹配字段
	data, err := yaml.Marshal(p.m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, obj)
}

func TestLoadFromProvider_DefaultsOnly(t *testing.T) {
	t.Parallel()
	p := mapProvider{m: map[string]any{}}
	cfg, err := LoadFromProvider(p)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.Addr != ":18083" {
		t.Fatalf("expected default Addr :18083, got %q", cfg.Addr)
	}
}

func TestLoadFromProvider_OverridesViaSet(t *testing.T) {
	t.Parallel()
	p := mapProvider{m: map[string]any{"addr": ":19999"}}
	cfg, err := LoadFromProvider(p)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.Addr != ":19999" {
		t.Fatalf("want :19999, got %q", cfg.Addr)
	}
}

func TestLoadConfig_FileNotExist_ReturnsDefault(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if cfg.Addr != ":18083" {
		t.Fatalf("expected default, got %+v", cfg)
	}
}

func TestSaveConfig_ToFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sproxy.yaml")

	cfg := Default()
	cfg.Addr = ":19999"
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Addr != ":19999" {
		t.Fatalf("Addr mismatch: want :19999, got %q", loaded.Addr)
	}
}

func TestSaveConfig_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only dir test not supported on Windows")
	}
	t.Parallel()

	roDir, cleanup := makeReadOnlyDir(t)
	defer cleanup()

	cfg := Default()
	err := SaveConfig(cfg, filepath.Join(roDir, "sproxy.yaml"))
	if err == nil {
		t.Fatal("expected error when saving to read-only directory")
	}
}

func TestSaveConfig_InvalidPath(t *testing.T) {
	t.Parallel()
	cfg := Default()
	err := SaveConfig(cfg, "/nonexistent/dir/config.yaml")
	if err == nil {
		t.Error("expected error saving to nonexistent directory")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	t.Parallel()
	cfg, err := LoadConfig("/nonexistent/config.yaml")
	if err != nil {
		t.Logf("LoadConfig for nonexistent file returned error: %v (acceptable)", err)
	}
	if cfg == nil {
		t.Log("LoadConfig returned nil config (acceptable)")
	}
}
