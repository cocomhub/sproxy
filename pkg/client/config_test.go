// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientConfig_Defaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ServerURL == "" {
		t.Error("DefaultConfig().ServerURL should not be empty")
	}
	if cfg.Timeout <= 0 {
		t.Error("DefaultConfig().Timeout should be positive")
	}
	if cfg.ChunkSize <= 0 {
		t.Error("DefaultConfig().ChunkSize should be positive")
	}
	// Validate should pass without error for default config
	if err := cfg.Validate(); err != nil {
		t.Errorf("Default config Validate() returned error: %v", err)
	}
}

func TestClientConfig_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")

	cfg := DefaultConfig()
	cfg.ServerURL = "https://example.com:8443"

	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL mismatch: got %q, want %q", loaded.ServerURL, cfg.ServerURL)
	}
	if loaded.Timeout != cfg.Timeout {
		t.Errorf("Timeout mismatch: got %d, want %d", loaded.Timeout, cfg.Timeout)
	}
	if loaded.ChunkSize != cfg.ChunkSize {
		t.Errorf("ChunkSize mismatch: got %d, want %d", loaded.ChunkSize, cfg.ChunkSize)
	}
}

func TestClientConfig_CustomValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")

	cfg := &Config{
		ServerURL:    "https://myserver.local:9090",
		TunnelKey:    strings.Repeat("a", 64),
		Timeout:      600,
		ChunkSize:    8388608, // 8 MiB
		MaxChunkSize: 16777216,
	}

	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL mismatch: got %q, want %q", loaded.ServerURL, cfg.ServerURL)
	}
	if loaded.TunnelKey != cfg.TunnelKey {
		t.Errorf("TunnelKey mismatch: got %q, want %q", loaded.TunnelKey, cfg.TunnelKey)
	}
	if loaded.Timeout != cfg.Timeout {
		t.Errorf("Timeout mismatch: got %d, want %d", loaded.Timeout, cfg.Timeout)
	}
	if loaded.ChunkSize != cfg.ChunkSize {
		t.Errorf("ChunkSize mismatch: got %d, want %d", loaded.ChunkSize, cfg.ChunkSize)
	}
	if loaded.MaxChunkSize != cfg.MaxChunkSize {
		t.Errorf("MaxChunkSize mismatch: got %d, want %d", loaded.MaxChunkSize, cfg.MaxChunkSize)
	}
}

func TestClientConfig_ValidateBadTunnelKey(t *testing.T) {
	// Tunnel key too short (8 chars instead of 64)
	cfg := DefaultConfig()
	cfg.TunnelKey = "shortkey"
	if err := cfg.Validate(); err == nil {
		t.Error("Expected error for short tunnel key, got nil")
	}

	// Tunnel key too long (128 chars instead of 64)
	cfg2 := DefaultConfig()
	cfg2.TunnelKey = strings.Repeat("b", 128)
	if err := cfg2.Validate(); err == nil {
		t.Error("Expected error for long tunnel key, got nil")
	}

	// Tunnel key not hex (64 chars but not hex) — Validate only checks length, not hex validity
	// Per the implementation, it only checks len(c.TunnelKey) != 64, so a 64-char non-hex key should pass
	cfg3 := DefaultConfig()
	cfg3.TunnelKey = strings.Repeat("x", 64) // 64 chars, but not hex
	if err := cfg3.Validate(); err != nil {
		t.Errorf("Expected no error for 64-char tunnel key (length check only), got: %v", err)
	}

	// Empty tunnel key should pass (optional field)
	cfg4 := DefaultConfig()
	cfg4.TunnelKey = ""
	if err := cfg4.Validate(); err != nil {
		t.Errorf("Expected no error for empty tunnel key, got: %v", err)
	}

	// Valid 64-char hex tunnel key should pass
	cfg5 := DefaultConfig()
	cfg5.TunnelKey = strings.Repeat("a", 64)
	if err := cfg5.Validate(); err != nil {
		t.Errorf("Expected no error for valid 64-char tunnel key, got: %v", err)
	}
}

// TestClientConfig_LoadNonExistent verifies that LoadConfig on a non-existent file
// creates a default config file and returns defaults (existing behavior).
func TestClientConfig_LoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	// File should not exist yet
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Expected file to not exist yet: %s", path)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on non-existent path failed: %v", err)
	}

	if cfg.ServerURL != "http://localhost:18083" {
		t.Errorf("Expected default ServerURL, got %q", cfg.ServerURL)
	}

	// File should now exist (LoadConfig created it with defaults)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("Expected LoadConfig to create default config file, but file does not exist")
	}
}