// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

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
}

func TestConfig_Validate_FillsZeroes(t *testing.T) {
	t.Parallel()
	c := &Config{}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Addr == "" || c.UploadsDir == "" || c.ChunkSize <= 0 ||
		c.UploadSessionTTL <= 0 || c.ServerTimeouts.Shutdown <= 0 {
		t.Fatalf("Validate did not fill zero values: %+v", c)
	}
}

func TestConfig_Validate_TunnelKey_HexCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty_ok", "", false},
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

func TestLoadFromViper_DefaultsOnly(t *testing.T) {
	t.Parallel()
	v := viper.New()
	cfg, err := LoadFromViper(v)
	if err != nil {
		t.Fatalf("LoadFromViper: %v", err)
	}
	if cfg.Addr != ":18083" {
		t.Fatalf("expected default Addr :18083, got %q", cfg.Addr)
	}
}

func TestLoadFromViper_OverridesViaSet(t *testing.T) {
	t.Parallel()
	v := viper.New()
	v.Set("addr", ":19999")
	cfg, err := LoadFromViper(v)
	if err != nil {
		t.Fatalf("LoadFromViper: %v", err)
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
