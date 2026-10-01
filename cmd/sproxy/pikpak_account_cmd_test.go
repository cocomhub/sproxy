// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdPikpakAccount_Manage 验证 account 子命令注册与增删查闭环（fake secrets 目录）。
func TestNewCmdPikpakAccount_Manage(t *testing.T) {
	t.Parallel()
	old := pikpakSecretsDirOverride
	pikpakSecretsDirOverride = t.TempDir()
	t.Cleanup(func() { pikpakSecretsDirOverride = old })

	ios := cli.IOStreams{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}

	// add：写入账号凭据（fake secrets 目录）。
	cmd := newCmdPikpakAccount(ios)
	var b strings.Builder
	cmd.SetOut(&b)
	cmd.SetArgs([]string{"add", "a1", `{"access_token":"t1","refresh_token":"r1"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("account add failed: %v", err)
	}
	if !strings.Contains(b.String(), `"a1"`) {
		t.Fatalf("expected add output mentioning account, got %s", b.String())
	}

	// list：应列出账号。
	b.Reset()
	cmd2 := newCmdPikpakAccount(ios)
	cmd2.SetOut(&b)
	cmd2.SetArgs([]string{"list"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("account list failed: %v", err)
	}
	if !strings.Contains(b.String(), "a1") {
		t.Fatalf("expected account a1 in list output, got %s", b.String())
	}

	// remove：删除账号。
	b.Reset()
	cmd3 := newCmdPikpakAccount(ios)
	cmd3.SetOut(&b)
	cmd3.SetArgs([]string{"remove", "a1"})
	if err := cmd3.Execute(); err != nil {
		t.Fatalf("account remove failed: %v", err)
	}
	if !strings.Contains(b.String(), "a1") {
		t.Fatalf("expected remove output, got %s", b.String())
	}

	// list：应无账号。
	b.Reset()
	cmd4 := newCmdPikpakAccount(ios)
	cmd4.SetOut(&b)
	cmd4.SetArgs([]string{"list"})
	if err := cmd4.Execute(); err != nil {
		t.Fatalf("account list after remove failed: %v", err)
	}
	if strings.Contains(b.String(), "a1") {
		t.Fatalf("expected no account after remove, got %s", b.String())
	}
}

// TestNewCmdPikpak_Help_IncludeAccount pikpak 帮助应含 account 子命令。
func TestNewCmdPikpak_Help_IncludeAccount(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ios := cli.IOStreams{Out: &out, ErrOut: &out}
	cmd := newCmdPikpak(ios)
	var b strings.Builder
	cmd.SetOut(&b)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"install", "login", "account"} {
		if !strings.Contains(b.String(), sub) {
			t.Fatalf("expected %s subcommand in help, got %s", sub, b.String())
		}
	}
}
