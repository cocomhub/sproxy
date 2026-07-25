// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestNewCmdGenkey(t *testing.T) {
	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: &buf}

	cmd := NewCmdGenkey(ios)
	if cmd.Use != "genkey" {
		t.Errorf("Use = %q, want %q", cmd.Use, "genkey")
	}
	if cmd.Short != "生成 tunnel_key 密钥" {
		t.Errorf("Short = %q, want %q", cmd.Short, "生成 tunnel_key 密钥")
	}

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v", err)
	}

	got := strings.TrimSpace(buf.String())
	// 密钥应为 64 个十六进制字符
	if len(got) != 64 {
		t.Errorf("output length = %d, want 64", len(got))
	}
	for _, ch := range got {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			t.Errorf("non-hex character %q in output", ch)
		}
	}
}
