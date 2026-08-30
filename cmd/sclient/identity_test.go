// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestIdentityCommand_Generate 验证 identity generate 创建身份文件并打印指纹。
func TestIdentityCommand_Generate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.json")
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityGenerate(ios)
	cmd.SetArgs([]string{"--file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("identity file not created: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Fingerprint: sha256:") {
		t.Fatalf("output missing fingerprint: %q", got)
	}
	id, err := tunnel.LoadIdentity(file)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !strings.Contains(got, id.Fingerprint()) {
		t.Fatalf("output fingerprint mismatch: %q", got)
	}
}

// TestIdentityCommand_Generate_ExistsWithoutForce 验证已存在时拒绝（无 --force）。
func TestIdentityCommand_Generate_ExistsWithoutForce(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.json")
	id, _ := tunnel.GenerateIdentity()
	if err := tunnel.SaveIdentity(id, file); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityGenerate(ios)
	cmd.SetArgs([]string{"--file", file})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for existing identity without --force")
	}
	if !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("expected exists error, got %v", err)
	}
	loaded, _ := tunnel.LoadIdentity(file)
	if loaded.Fingerprint() != id.Fingerprint() {
		t.Fatal("identity file should not change without --force")
	}
}

// TestIdentityCommand_Generate_ForceOverwrites 验证 --force 覆盖已有身份。
func TestIdentityCommand_Generate_ForceOverwrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.json")
	id, _ := tunnel.GenerateIdentity()
	if err := tunnel.SaveIdentity(id, file); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityGenerate(ios)
	cmd.SetArgs([]string{"--file", file, "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	loaded, _ := tunnel.LoadIdentity(file)
	if loaded.Fingerprint() == id.Fingerprint() {
		t.Fatal("--force should overwrite identity")
	}
}

// TestIdentityCommand_Show 验证 identity show 打印指纹与公钥。
func TestIdentityCommand_Show(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.json")
	id, _ := tunnel.GenerateIdentity()
	if err := tunnel.SaveIdentity(id, file); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityShow(ios)
	cmd.SetArgs([]string{"--file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Fingerprint: "+id.Fingerprint()) {
		t.Fatalf("missing fingerprint: %q", got)
	}
}

// TestIdentityCommand_Show_NotExist 验证 show 时身份文件不存在给出清晰提示。
func TestIdentityCommand_Show_NotExist(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing.json")
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityShow(ios)
	cmd.SetArgs([]string{"--file", file})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing identity")
	}
	if !strings.Contains(err.Error(), "身份文件不存在") {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}

// TestIdentityCommand_Fingerprint 验证 identity fingerprint 仅打印指纹。
func TestIdentityCommand_Fingerprint(t *testing.T) {
	file := filepath.Join(t.TempDir(), "identity.json")
	id, _ := tunnel.GenerateIdentity()
	if err := tunnel.SaveIdentity(id, file); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	ios := cli.IOStreams{Out: &out, ErrOut: &errOut}

	cmd := NewCmdIdentityFingerprint(ios)
	cmd.SetArgs([]string{"--file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if got != id.Fingerprint() {
		t.Fatalf("expected %q, got %q", id.Fingerprint(), got)
	}
}

// TestIdentityCommand_ParentHasSubcommands 验证 identity 父命令挂载子命令。
func TestIdentityCommand_ParentHasSubcommands(t *testing.T) {
	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: &buf}
	cmd := NewCmdIdentity(ios)
	if cmd.Use != "identity" {
		t.Errorf("Use = %q, want %q", cmd.Use, "identity")
	}
	names := map[string]bool{}
	for _, sub := range cmd.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"generate", "show", "fingerprint"} {
		if !names[want] {
			t.Errorf("identity missing subcommand %q", want)
		}
	}
}
