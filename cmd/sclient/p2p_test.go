// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
)

func TestNewCmdP2P_Subcommands(t *testing.T) {
	cmd := NewCmdP2P()
	if cmd.Use != "p2p" {
		t.Fatalf("expected Use 'p2p', got %q", cmd.Use)
	}
	subs := map[string]bool{"connect": false, "listen": false}
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

func TestNewCmdP2PConnect_Flags(t *testing.T) {
	cmd := NewCmdP2P()
	connect := cmd.Commands()[0]
	for _, name := range []string{"peer", "tcp", "listen", "hub", "token", "node-id"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("p2p connect 缺少 flag: %s", name)
		}
	}
}
