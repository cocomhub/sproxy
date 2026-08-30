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
)

func TestNewCmdSocks_Flags(t *testing.T) {
	cmd := newCmdSocks(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, nil)
	if cmd.Use != "socks [-l :port] --exit <node>" {
		t.Fatalf("unexpected Use: %q", cmd.Use)
	}
	for _, name := range []string{"listen", "exit", "gateway", "mdns", "mdns-secret", "socks-user", "socks-pass", "webrtc", "hub", "node-id"} {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("socks 缺少 flag: %s", name)
		}
	}
}

// TestSocks_ExitRequired：--exit 未提供时报错（明确指定出口节点，防未授权代理）。
func TestSocks_ExitRequired(t *testing.T) {
	cmd := newCmdSocks(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("缺少 --exit 应报错")
	}
	if !strings.Contains(err.Error(), "--exit") {
		t.Fatalf("错误信息应提示 --exit, got: %v", err)
	}
}
