// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestVersionCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &testConfigProvider{})
	if !strings.HasPrefix(cmd.Use, "version") {
		t.Errorf("expected Use to start with 'version', got %q", cmd.Use)
	}
}

func TestVersionCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &testConfigProvider{})
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	// 注意："version" 命令既是"显示程序版本"又是"文件版本管理"的入口。
	// 这里的子命令是文件版本管理（list/restore/delete），而非程序版本子命令。
	for _, name := range []string{"list", "restore", "delete"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestVersionCmd_ShowVersion(t *testing.T) {
	Version = "1.2.3"
	BuildAt = "2026-07-26T12:00:00Z"
	t.Cleanup(func() {
		Version = "dev"
		BuildAt = "unknown"
	})

	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	var buf strings.Builder
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, cfgSvc)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "1.2.3") {
		t.Errorf("expected output to contain version '1.2.3', got: %s", output)
	}
	if !strings.Contains(output, "2026-07-26T12:00:00Z") {
		t.Errorf("expected output to contain build time, got: %s", output)
	}
}

func TestVersionCmd_NilConfigSvc(t *testing.T) {
	Version = "1.0.0"
	BuildAt = "test-build"
	t.Cleanup(func() {
		Version = "dev"
		BuildAt = "unknown"
	})

	// 使用返回空配置的 testConfigProvider，而非 nil 指针
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	var buf strings.Builder
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, cfgSvc)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("expected output to contain version, got: %s", output)
	}
}
