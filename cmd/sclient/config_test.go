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
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// TestConfigCmd_ShowResolvedContext：config.yaml（context 模型）存在且可解析时，
// config show 输出**当前 context 解析后的扁平视图**（server_url/access_key 来自
// env+user；access_key_secret 脱敏）。红灯：现实现读平铺 cfgSvc，不走 context。
func TestConfigCmd_ShowResolvedContext(t *testing.T) {
	t.Parallel()
	cfgPath := writeContextFixture(t)
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()} // 平铺 cfgSvc 应被 context 合成覆盖
	var buf strings.Builder
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &cfgPath, cfgSvc)
	err := cmd.RunE(cmd, []string{"show"})
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	o := buf.String()
	if !strings.Contains(o, "https://a:18083") {
		t.Errorf("show 应含当前 context env 的 server_url（https://a:18083）, got: %s", o)
	}
	if !strings.Contains(o, "ak-1") {
		t.Errorf("show 应含当前 context user 的 access_key（ak-1）, got: %s", o)
	}
	// 脱敏：secret 不得明文出现（fixture 的合法 hex 不应输出）。
	if strings.Contains(o, "6162636465666768696a6b6c6d6e6f707172737475767778797a303132333435") {
		t.Errorf("show 不应泄露 access_key_secret 明文: %s", o)
	}
}

// TestConfigCmd_SetResolvedContext：config.yaml 存在时 config set server_url
// 写**当前 context 的 env 段**（而非平铺文件）。
func TestConfigCmd_SetResolvedContext(t *testing.T) {
	t.Parallel()
	cfgPath := writeContextFixture(t)
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	var buf strings.Builder
	cmd := NewCmdConfig(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &cfgPath, cfgSvc)
	err := cmd.RunE(cmd, []string{"set", "server_url", "https://updated:18083"})
	if err != nil {
		t.Fatalf("config set: %v", err)
	}
	cc, cerr := contextcfg.Load(cfgPath)
	if cerr != nil {
		t.Fatalf("Load config.yaml: %v", cerr)
	}
	env := cc.FindEnvironment("a")
	if env == nil || env.ServerURL != "https://updated:18083" {
		t.Errorf("config set server_url 应写当前 context 的 env 段: %+v", env)
	}
}

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
