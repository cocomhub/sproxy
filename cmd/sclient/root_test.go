// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig（P2-配置2）：
// SCLIENT_ENV 环境变量选择 env 后缀配置文件（sclient.prod.yaml）作为 --config 默认值。
func TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "prod")
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.Contains(flag.DefValue, "sclient.prod.yaml") {
		t.Fatalf("--config 默认值应含 sclient.prod.yaml，got %q", flag.DefValue)
	}
}

// TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig（P2-配置2）：
// 未设置 SCLIENT_ENV 时用默认 sclient.yaml。
func TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.Contains(flag.DefValue, "sclient.yaml") || strings.Contains(flag.DefValue, "sclient.prod.yaml") {
		t.Fatalf("--config 默认值应含 sclient.yaml 且不含 env 后缀，got %q", flag.DefValue)
	}
}

func TestLoadConfig_NilProvider(t *testing.T) {
	svc := &cliConfigProvider{provider: nil}
	_, err := svc.LoadConfig()
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestLoadConfig_WithProvider(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	vp.Set("server_url", "http://test:18083")
	svc := &cliConfigProvider{provider: vp}
	cfg, err := svc.LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerURL != "http://test:18083" {
		t.Errorf("expected server_url 'http://test:18083', got %q", cfg.ServerURL)
	}
}

func TestLoadConfig_WithProviderDefaults(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	svc := &cliConfigProvider{provider: vp}
	cfg, err := svc.LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerURL != client.DefaultConfig().ServerURL {
		t.Errorf("expected default server_url %q, got %q", client.DefaultConfig().ServerURL, cfg.ServerURL)
	}
}

func TestExecute_Help(t *testing.T) {
	// Execute() creates a new root cmd and runs it. Without args it should show help and return nil.
	err := Execute()
	if err != nil {
		t.Fatalf("unexpected error from Execute() (help should not return error): %v", err)
	}
}
