// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestNewCmdHTTPProxy_Flags(t *testing.T) {
	t.Parallel()
	cmd := newCmdHTTPProxy(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	for _, name := range []string{"listen", "proxy-user", "proxy-pass"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("http-proxy 缺少 flag: --%s", name)
		}
	}
	// mesh 参数组已收敛到 meshconn.AddFlags
	for _, name := range []string{"exit", "exit-auto", "exit-only", "exit-exclude", "local-timeout"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("http-proxy 缺少 mesh 参数组 flag: --%s", name)
		}
	}
}

func TestNewCmdHTTPProxy_Help(t *testing.T) {
	t.Parallel()
	cmd := newCmdHTTPProxy(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	if !strings.Contains(cmd.Short, "HTTP 代理") {
		t.Fatalf("Short = %q, want 含 HTTP 代理", cmd.Short)
	}
}
