// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

// TestNewCmdPikpak_Help 验证 pikpak 子命令注册。
func TestNewCmdPikpak_Help(t *testing.T) {
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
	if !strings.Contains(b.String(), "install") || !strings.Contains(b.String(), "login") {
		t.Fatalf("expected subcommands in help, got %s", b.String())
	}
}
