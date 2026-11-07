// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// fakeSecretStore 是 account 命令测试用内存凭据存储（SecretStore 接口，替代加密卷）。
type fakeSecretStore struct {
	data map[string][]byte
}

func newFakeSecretStore() *fakeSecretStore { return &fakeSecretStore{data: map[string][]byte{}} }

func (s *fakeSecretStore) Read(_ context.Context, name string) ([]byte, error) {
	b, ok := s.data[name]
	if !ok {
		return nil, fmt.Errorf("not found %s", name)
	}
	return b, nil
}

func (s *fakeSecretStore) Write(_ context.Context, name string, data []byte) error {
	s.data[name] = append([]byte(nil), data...)
	return nil
}

func (s *fakeSecretStore) Delete(_ context.Context, name string) error {
	delete(s.data, name)
	return nil
}

func (s *fakeSecretStore) List(_ context.Context) ([]string, error) {
	names := make([]string, 0, len(s.data))
	for n := range s.data {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// TestNewCmdPikpakAccount_Manage 验证 account 子命令注册与增删查闭环
// （凭据存储经构造传参注入 fake + 临时状态目录 → 各测试独立，可安全并行）。
func TestNewCmdPikpakAccount_Manage(t *testing.T) {
	t.Parallel()
	store := newFakeSecretStore()
	stateDir := t.TempDir()
	ios := cli.IOStreams{In: strings.NewReader(`{"access_token":"t1","refresh_token":"r1"}`), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}

	// add：写入账号凭据（fake 存储；凭据从 stdin 读，防 argv 泄漏）。
	cmd := newCmdPikpakAccount(ios, store, stateDir)
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
	cmd2 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, store, stateDir)
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
	cmd3 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, store, stateDir)
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
	cmd4 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, store, stateDir)
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
// 凭据存储经构造传参注入共享 fake → 可安全并行。
func TestNewCmdPikpakAccount_AddDuplicateCrossProcess(t *testing.T) {
	t.Parallel()
	store := newFakeSecretStore()
	stateDir := t.TempDir()

	var b1 strings.Builder
	cmd1 := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader(`{"access_token":"t2"}`), Out: &b1, ErrOut: io.Discard}, store, stateDir)
	cmd1.SetOut(&b1)
	cmd1.SetArgs([]string{"add", "dup"})
	if err := cmd1.Execute(); err != nil {
		t.Fatalf("first add failed: %v", err)
	}

	// 第二个进程（同存储，重新构造任何状态）：LoadAccounts 后 Add 必须拒绝重名。
	var b2 bytes.Buffer
	cmd2 := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader("{\"access_token\":\"t3\"}"), Out: &b2, ErrOut: io.Discard}, store, stateDir)
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

// TestNewCmdPikpakAccount_RejectsPathTraversal add 非法账号名（含路径分隔符/..）必须
// fail-closed（CWE-22 防御；账号名参与加密卷 secret 文件名拼装）。
func TestNewCmdPikpakAccount_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"../evil", "a/b", `a\b`, "..", "."} {
		store := newFakeSecretStore()
		var b strings.Builder
		cmd := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader(`{"a":1}`), Out: &b, ErrOut: io.Discard}, store, t.TempDir())
		cmd.SetOut(&b)
		cmd.SetArgs([]string{"add", bad})
		if err := cmd.Execute(); err == nil {
			t.Fatalf("非法账号名 %q 应被拒绝（路径穿越防御）", bad)
		}
		if len(store.data) != 0 {
			t.Fatalf("非法账号名 %q 不应写入任何 secret，got %v", bad, store.data)
		}
	}
}

// TestNewCmdPikpakAccount_ListUsesDerivedSecretName list 输出展示的 secret 名由账号名派生
// （pikpak-<name>.json），不依赖已移除的 SecretURL 字段。
func TestNewCmdPikpakAccount_ListUsesDerivedSecretName(t *testing.T) {
	t.Parallel()
	store := newFakeSecretStore()
	stateDir := t.TempDir()
	var b strings.Builder
	cmd := newCmdPikpakAccount(cli.IOStreams{In: strings.NewReader(`{"access_token":"t4"}`), Out: &b, ErrOut: io.Discard}, store, stateDir)
	cmd.SetOut(&b)
	cmd.SetArgs([]string{"add", "acct-01"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	b.Reset()
	cmd2 := newCmdPikpakAccount(cli.IOStreams{Out: &b, ErrOut: io.Discard}, store, stateDir)
	cmd2.SetOut(&b)
	cmd2.SetArgs([]string{"list"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(b.String(), "pikpak-acct-01.json") {
		t.Fatalf("expected derived secret name in list, got %s", b.String())
	}
}
