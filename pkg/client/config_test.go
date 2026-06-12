// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/spf13/viper"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	// valid config — all fields set, no tunnel_key → no error
	cfg := &Config{ServerURL: "http://localhost:8080", Timeout: 30, ChunkSize: size.DefaultChunkSize}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on valid config: %v", err)
	}

	// empty ServerURL → defaults to localhost
	cfg2 := &Config{Timeout: 30}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate() on config with empty ServerURL: %v", err)
	}
	if cfg2.ServerURL != "http://localhost:18083" {
		t.Errorf("expected ServerURL to default, got %q", cfg2.ServerURL)
	}

	// zero Timeout → defaults to 300
	cfg3 := &Config{ServerURL: "http://x", Timeout: 0}
	if err := cfg3.Validate(); err != nil {
		t.Fatalf("Validate() on config with zero Timeout: %v", err)
	}
	if cfg3.Timeout != 300 {
		t.Errorf("expected Timeout to default to 300, got %d", cfg3.Timeout)
	}

	// zero ChunkSize → defaults to DefaultChunkSize
	cfg4 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: 0}
	if err := cfg4.Validate(); err != nil {
		t.Fatalf("Validate() on config with zero ChunkSize: %v", err)
	}
	if cfg4.ChunkSize != size.DefaultChunkSize {
		t.Errorf("expected ChunkSize to default to %d, got %d", size.DefaultChunkSize, cfg4.ChunkSize)
	}

	// invalid tunnel_key length → error
	cfg5 := &Config{ServerURL: "http://x", Timeout: 30, TunnelKey: "too-short"}
	if err := cfg5.Validate(); err == nil {
		t.Fatal("expected error for invalid tunnel_key length, got nil")
	}

	// valid tunnel_key length → no error
	cfg6 := &Config{ServerURL: "http://x", Timeout: 30, TunnelKey: strings.Repeat("a", 64)}
	if err := cfg6.Validate(); err != nil {
		t.Fatalf("Validate() on config with 64-char tunnel_key: %v", err)
	}
}

func TestLoadFromViper(t *testing.T) {
	t.Parallel()

	v := viper.New()
	v.Set("server_url", "http://test:8080")
	v.Set("timeout", 60)

	cfg, err := LoadFromViper(v)
	if err != nil {
		t.Fatalf("LoadFromViper: %v", err)
	}
	if cfg.ServerURL != "http://test:8080" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "http://test:8080")
	}
	if cfg.Timeout != 60 {
		t.Errorf("Timeout = %d, want %d", cfg.Timeout, 60)
	}
	if cfg.ChunkSize != size.DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want %d", cfg.ChunkSize, size.DefaultChunkSize)
	}
}

func TestLoadFromViper_InvalidTunnelKey(t *testing.T) {
	t.Parallel()

	v := viper.New()
	v.Set("server_url", "http://test:8080")
	v.Set("tunnel_key", "bad-key")

	_, err := LoadFromViper(v)
	if err == nil {
		t.Fatal("expected error for invalid tunnel_key, got nil")
	}
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if cfg.ServerURL != "http://localhost:18083" {
		t.Errorf("expected default ServerURL, got %q", cfg.ServerURL)
	}
}

func TestLoadConfig_NonexistentPath(t *testing.T) {
	dir := t.TempDir()
	// Use a path where parent dir exists but the file itself does not
	path := filepath.Join(dir, "sclient.yaml")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on nonexistent path should not error, got: %v", err)
	}
	if cfg.ServerURL != "http://localhost:18083" {
		t.Errorf("expected default ServerURL, got %q", cfg.ServerURL)
	}
	// config file should have been created
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("expected LoadConfig to create default config file at %s", path)
	}
}

func TestLoadConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")
	content := "server_url: https://example.com\ntimeout: 99\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "https://example.com" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "https://example.com")
	}
	if cfg.Timeout != 99 {
		t.Errorf("Timeout = %d, want %d", cfg.Timeout, 99)
	}
}

func TestHandleConfigShow(t *testing.T) {
	// HandleConfigShow prints to stdout — verify it doesn't panic
	cfg := DefaultConfig()
	cfg.ServerURL = "https://example.com"
	cfg.TunnelKey = strings.Repeat("d", 64)
	cfg.ChunkSize = 8 << 20
	cfg.MaxChunkSize = 32 << 20

	HandleConfigShow(cfg)
}
