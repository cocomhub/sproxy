// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestHandleConfigSet_ServerURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := DefaultConfig()
	if err := HandleConfigSet(cfg, path, "server_url", "https://example.com:8443"); err != nil {
		t.Fatalf("HandleConfigSet: %v", err)
	}
	if cfg.ServerURL != "https://example.com:8443" {
		t.Errorf("ServerURL: got %q, want %q", cfg.ServerURL, "https://example.com:8443")
	}
}

func TestHandleConfigSet_TunnelKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := DefaultConfig()
	if err := HandleConfigSet(cfg, path, "tunnel_key", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("HandleConfigSet: %v", err)
	}
	if cfg.TunnelKey != strings.Repeat("b", 64) {
		t.Errorf("TunnelKey: got %q, want %q", cfg.TunnelKey, strings.Repeat("b", 64))
	}
}

func TestHandleConfigSet_InvalidKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := DefaultConfig()
	if err := HandleConfigSet(cfg, path, "nonexistent_key", "value"); err == nil {
		t.Error("expected error for nonexistent key, got nil")
	}
}

func TestConfigFileSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg := DefaultConfig()
	cfg.ServerURL = "https://test.example.com"
	cfg.TunnelKey = strings.Repeat("c", 64)

	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig error: %v", err)
	}

	loaded := DefaultConfig()
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		t.Fatalf("viper.ReadInConfig error: %v", err)
	}
	if err := v.Unmarshal(loaded); err != nil {
		t.Fatalf("viper.Unmarshal error: %v", err)
	}

	if loaded.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL: got %q, want %q", loaded.ServerURL, cfg.ServerURL)
	}
	if loaded.TunnelKey != cfg.TunnelKey {
		t.Errorf("TunnelKey changed after save/load: got %q, want %q", loaded.TunnelKey, cfg.TunnelKey)
	}
}