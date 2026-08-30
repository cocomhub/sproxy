// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

func TestNewCmdUDP_SubcommandAndFlags(t *testing.T) {
	cmd := newCmdUDP(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	if cmd.Use != "udp" {
		t.Fatalf("unexpected Use: %q", cmd.Use)
	}
	var mapCmd *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "map" {
			mapCmd = c
			break
		}
	}
	if mapCmd == nil {
		t.Fatal("udp 缺少 map 子命令")
	}
	for _, name := range []string{"listen", "exit", "remote", "mdns", "mdns-secret", "hub", "node-id"} {
		if f := mapCmd.Flags().Lookup(name); f == nil {
			t.Errorf("udp map 缺少 flag: %s", name)
		}
	}
}

// TestUDPMap_RequiredFlags：--exit/--remote 缺失时报错（明确指定出口与远程目标）。
func TestUDPMap_RequiredFlags(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	cmd := newCmdUDP(clientfactory.NewMock(nil, nil), ios, nil)
	mapCmd := cmd.Commands()[0]
	mapCmd.SetContext(context.Background())
	err := mapCmd.RunE(mapCmd, nil)
	if err == nil {
		t.Fatal("缺少 --exit/--remote 应报错")
	}
	if !strings.Contains(err.Error(), "--exit") || !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("错误信息应提示 --exit/--remote, got: %v", err)
	}
}
