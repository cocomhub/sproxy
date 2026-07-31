// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestConfigCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, new(string), &testConfigProvider{})
	if !strings.HasPrefix(cmd.Use, "config") {
		t.Errorf("expected Use to start with 'config', got %q", cmd.Use)
	}
}

func TestConfigCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, new(string), &testConfigProvider{})
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	if !names["remote"] {
		t.Error("expected subcommand 'remote', not found")
	}
}

func TestConfigCmd_ShowWithConfig(t *testing.T) {
	cfgSvc := &testConfigProvider{cfg: &client.Config{ServerURL: "http://test:18083"}}
	var buf strings.Builder
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, new(string), cfgSvc)
	err := cmd.RunE(cmd, []string{"show"})
	if err != nil {
		t.Fatalf("config show command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "http://test:18083") {
		t.Errorf("expected output to contain server URL, got: %s", buf.String())
	}
}

func TestConfigCmd_Set(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "sclient.yaml")

	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &cfgPath, cfgSvc)
	// 直接调用 RunE，避免 cobra subcommand 路由（set 不是子命令而是 positional arg）
	err := cmd.RunE(cmd, []string{"set", "server_url", "http://new:18083"})
	if err != nil {
		t.Fatalf("config set failed: %v", err)
	}

	// 验证文件已被写入磁盘且内容正确
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if !strings.Contains(string(data), "http://new:18083") {
		t.Errorf("expected config file to contain new server_url, got: %s", string(data))
	}
}

func TestConfigCmd_SetMissingArgs(t *testing.T) {
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, new(string), cfgSvc)
	err := cmd.RunE(cmd, []string{"set", "server_url"})
	if err == nil {
		t.Error("expected error when set has no value")
	}
}

func TestConfigCmd_UnknownSubcommand(t *testing.T) {
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, new(string), cfgSvc)
	err := cmd.RunE(cmd, []string{"unknown"})
	if err == nil {
		t.Error("expected error for unknown subcommand")
	}
}
