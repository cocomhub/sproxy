// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestNewCmdAccessKeyCreate(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdAccessKey(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"create"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("access-key create failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "AccessKey:") {
		t.Errorf("expected output to contain 'AccessKey:' line, got: %s", out)
	}
	if !strings.Contains(out, "AccessKeySecret:") {
		t.Errorf("expected output to contain 'AccessKeySecret:' line, got: %s", out)
	}
}

func TestNewCmdAccessKeyCreate_MeshPrefix(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdAccessKey(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"create", "--mesh", "meshA"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("access-key create --mesh failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "AccessKey:       sk-meshA-") {
		t.Errorf("expected AccessKey with sk-meshA- prefix, got: %s", out)
	}
}

func TestGenerateAccessKeyPair(t *testing.T) {
	t.Parallel()
	ak, sk, err := generateAccessKeyPair("")
	if err != nil {
		t.Fatalf("generateAccessKeyPair failed: %v", err)
	}
	if !strings.HasPrefix(ak, "sk-") {
		t.Errorf("expected AccessKey to start with sk-, got: %s", ak)
	}
	// sk 应为 32B 随机 hex（64 个 hex 字符）
	if len(sk) != 64 {
		t.Errorf("expected AccessKeySecret to be 64 hex chars, got %d", len(sk))
	}
	// 两次生成应不同（随机性）
	ak2, sk2, err := generateAccessKeyPair("")
	if err != nil {
		t.Fatalf("second generateAccessKeyPair failed: %v", err)
	}
	if ak == ak2 || sk == sk2 {
		t.Error("expected two generated pairs to differ")
	}
}
