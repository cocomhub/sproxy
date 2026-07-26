// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/client"
)

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
