// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestNewVersionSubcommand 验证 version 子命令输出包含默认版本 "dev"。
func TestNewVersionSubcommand(t *testing.T) {
	cmd := NewVersionSubcommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version subcommand execute failed: %v", err)
	}
	if !strings.Contains(buf.String(), "dev") {
		t.Errorf("expected version output to contain %q, got: %s", "dev", buf.String())
	}
}

// TestNewVersionSubcommand_DirtyInfo 验证 dirty-info 子命令存在且执行无错。
func TestNewVersionSubcommand_DirtyInfo(t *testing.T) {
	cmd := NewVersionSubcommand()
	dirty := cmd.Commands()
	var found bool
	for _, c := range dirty {
		if c.Name() == "dirty-info" {
			found = true
			var buf bytes.Buffer
			c.SetOut(&buf)
			if err := c.Execute(); err != nil {
				t.Fatalf("dirty-info subcommand execute failed: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("expected dirty-info subcommand")
	}
}

// TestNewVersionSubcommand_JSON 验证 --json 输出可反序列化为 map 且含 "Version" 键。
func TestNewVersionSubcommand_JSON(t *testing.T) {
	cmd := NewVersionSubcommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version --json execute failed: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v (output: %q)", err, buf.String())
	}
	if _, ok := m["Version"]; !ok {
		t.Errorf("expected JSON output to contain \"Version\" key, got: %s", buf.String())
	}
}
