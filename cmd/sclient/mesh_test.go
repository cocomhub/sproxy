// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestNewCmdMesh_Subcommands(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Use != "mesh" {
		t.Fatalf("expected Use 'mesh', got %q", cmd.Use)
	}
	subs := map[string]bool{"connect": false, "status": false}
	for _, c := range cmd.Commands() {
		if _, ok := subs[c.Name()]; ok {
			subs[c.Name()] = true
		}
	}
	for name, found := range subs {
		if !found {
			t.Errorf("missing subcommand: %s", name)
		}
	}
}

func TestNewCmdMeshConnect_ArgsAndFlags(t *testing.T) {
	cmd := NewCmdMesh(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	connect := cmd.Commands()[0]
	if connect.Use != "connect <service> [-l :port]" {
		t.Fatalf("unexpected connect Use: %q", connect.Use)
	}
	if f := connect.Flags().Lookup("listen"); f == nil {
		t.Error("connect 缺少 -l/--listen flag")
	}
}
