// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdPikpakAccount_Manage 验证 account 子命令注册与增删查闭环
// （secrets 目录经构造传参注入临时目录 → 各测试独立，可安全并行）。
func TestNewCmdPikpakAccount_Manage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ios := cli.IOStreams{In: strings.NewReader(`{"access_token":"t1","refresh_token":"r1"}`), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}

	// add：写入账号凭据（临时 secrets 目录；凭据从 stdin 读，防 argv 泄漏）。
	cmd := newCmdPikpakAccount(ios, dir)
	var b strings.Builder
	cmd.SetOut(&b)
	cmd.SetArgs([]string{"add", "a1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("account add failed: %v", err)
	}
	if !strings.Contains(b.String(), `"a1"`) {
		t.Fatalf("expected add output mentioning account, got %s", b.String())
	}

	// list：应列出账号（list 必须 LoadAccounts，此时只剩 a1）。
	b.Reset()
	cmd2 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, dir)
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
	cmd3 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, dir)
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
	cmd4 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, dir)
	cmd4.SetOut(&b)
	cmd4.SetArgs([]string{"list"})
	if err := cmd4.Execute(); err != nil {
		t.Fatalf("account list after remove failed: %v", err)
	}
	if strings.Contains(b.String(), "a1") {
		t.Fatalf("expected no account after remove, got %s", b.String())
	}
}

// TestNewCmdPikpakAccount_AddDuplicateCrossProcess 跨进程重名账号：第二个进程 add
// 同名账号时必须报错（先 LoadAccounts 再 Add），不得静默覆盖 secrets 文件。
// secrets 目录经构造传参注入 → 可安全并行。
func TestNewCmdPikpakAccount_AddDuplicateCrossProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var b1 strings.Builder
	cmd1 := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader(`{"a":1}`), Out: &b1, ErrOut: io.Discard}, dir)
	cmd1.SetOut(&b1)
	cmd1.SetArgs([]string{"add", "dup"})
	if err := cmd1.Execute(); err != nil {
		t.Fatalf("first add failed: %v", err)
	}

	// 第二个进程（同 secrets 目录，重新构造任何状态）：LoadAccounts 后 Add 必须拒绝重名。
	var b2 bytes.Buffer
	cmd2 := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader("{\"v2\":2}"), Out: &b2, ErrOut: io.Discard}, dir)
	cmd2.SetOut(&b2)
	cmd2.SetArgs([]string{"add", "dup"})
	if err := cmd2.Execute(); err == nil {
		t.Fatalf("expected duplicate account error, got nil")
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
