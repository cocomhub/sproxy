// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sclientcfg_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/provider"
)

func TestNew_NoConfigFile(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if vp == nil {
		t.Fatal("New() 文件不存在时应返回非 nil ViperProvider")
	}
	var cfg struct {
		ServerURL string `mapstructure:"server_url"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 空配置不应报错: %v", err)
	}
}

func TestNew_WithConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
server_url: "https://example.com:18083"
chunk_size: 8388608
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sclientcfg.New(cfgPath)
	if vp == nil {
		t.Fatal("New() 有效配置文件时应返回非 nil ViperProvider")
	}

	var cfg struct {
		ServerURL string `mapstructure:"server_url"`
		ChunkSize int64  `mapstructure:"chunk_size"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.ServerURL != "https://example.com:18083" {
		t.Errorf("server_url = %q, want %q", cfg.ServerURL, "https://example.com:18083")
	}
	if cfg.ChunkSize != 8388608 {
		t.Errorf("chunk_size = %d, want %d", cfg.ChunkSize, 8388608)
	}
}

func TestRefresh(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yamlInitial := `server_url: "http://old.example.com:8080"`
	if err := os.WriteFile(cfgPath, []byte(yamlInitial), 0644); err != nil {
		t.Fatal(err)
	}

	vp := sclientcfg.New(cfgPath)

	yamlUpdated := `server_url: "http://new.example.com:9090"`
	if err := os.WriteFile(cfgPath, []byte(yamlUpdated), 0644); err != nil {
		t.Fatal(err)
	}

	if err := vp.Refresh(); err != nil {
		t.Fatalf("Refresh 应成功: %v", err)
	}

	var cfg struct {
		ServerURL string `mapstructure:"server_url"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.ServerURL != "http://new.example.com:9090" {
		t.Errorf("Refresh 后 server_url = %q, want %q", cfg.ServerURL, "http://new.example.com:9090")
	}
}

func TestSet_and_Unmarshal(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))

	vp.Set("server_url", "http://set.example.com:7777")
	vp.Set("chunk_size", 16777216)

	var cfg struct {
		ServerURL string `mapstructure:"server_url"`
		ChunkSize int    `mapstructure:"chunk_size"`
	}
	if err := vp.Unmarshal(&cfg); err != nil {
		t.Fatalf("Unmarshal 应成功: %v", err)
	}
	if cfg.ServerURL != "http://set.example.com:7777" {
		t.Errorf("server_url = %q, want %q", cfg.ServerURL, "http://set.example.com:7777")
	}
	if cfg.ChunkSize != 16777216 {
		t.Errorf("chunk_size = %d, want %d", cfg.ChunkSize, 16777216)
	}
}

func TestInterfaceCheck(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))

	var _ provider.Provider = vp
	var _ provider.Refresher = vp
}
