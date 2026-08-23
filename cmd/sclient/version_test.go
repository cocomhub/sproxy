// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

func TestVersionCmd_ProgramVersion(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdVersion(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetOut(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "dev") {
		t.Errorf("expected output to contain default version 'dev', got: %s", buf.String())
	}
}

func TestVersionCmd_DirtyInfo(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdVersion(cli.IOStreams{Out: &buf, ErrOut: io.Discard})

	var dirty *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "dirty-info" {
			dirty = c
			break
		}
	}
	if dirty == nil {
		t.Fatal("expected 'dirty-info' subcommand, not found")
	}

	dirty.SetOut(&buf)
	dirty.SetArgs(nil)
	if err := dirty.Execute(); err != nil {
		t.Fatalf("dirty-info command failed: %v", err)
	}
	// buildmeta.DirtyInfo 可能为空串（干净工作区），只断言命令存在且不 panic/不报错。
}

func TestVersionCmd_JSON(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdVersion(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version --json failed: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v, got: %s", err, buf.String())
	}
	if _, ok := m["Version"]; !ok {
		t.Errorf("expected JSON key 'Version', got: %v", m)
	}
}
