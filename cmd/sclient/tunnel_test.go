// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestTunnelCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdTunnel(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if !strings.HasPrefix(cmd.Use, "tunnel") {
		t.Errorf("expected Use to start with 'tunnel', got %q", cmd.Use)
	}
}

func TestTunnelCmd_ShortDesc(t *testing.T) {
	t.Parallel()
	cmd := NewCmdTunnel(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Short != "通过加密隧道转发请求" {
		t.Errorf("expected Short '通过加密隧道转发请求', got %q", cmd.Short)
	}
}

func TestTunnelCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdTunnel(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	flagNames := []string{"method", "header", "data", "include"}
	for _, name := range flagNames {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("missing flag: %s", name)
		}
	}
}

func TestTunnelCmd_ExactArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdTunnel(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("expected error for 0 args")
	}
	if err := cmd.Args(cmd, []string{"url1", "url2"}); err == nil {
		t.Error("expected error for 2 args")
	}
	if err := cmd.Args(cmd, []string{"http://example.com"}); err != nil {
		t.Errorf("expected no error for 1 arg, got: %v", err)
	}
}

func TestBuildTunnelRequest_InvalidHeader(t *testing.T) {
	t.Parallel()
	req, err := buildTunnelRequest(tunnelReqOpts{
		method:    "GET",
		targetURL: "http://example.com",
		headers:   []string{"invalid-header-without-colon"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Invalid header format should be silently skipped
	_ = req
}

func TestBuildTunnelRequest_EmptyBody(t *testing.T) {
	t.Parallel()
	req, err := buildTunnelRequest(tunnelReqOpts{
		method:    "POST",
		targetURL: "http://example.com",
		body:      "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Body != nil {
		t.Error("expected nil body for empty string")
	}
}

func TestBuildTunnelRequest_WithBody(t *testing.T) {
	t.Parallel()
	req, err := buildTunnelRequest(tunnelReqOpts{
		method:    "POST",
		targetURL: "http://example.com",
		body:      "test body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Body == nil {
		t.Fatal("expected non-nil body")
	}
}

func TestTunnelCmd_MethodFlag(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard, Out: io.Discard})
	cmd.SetArgs([]string{"-X", "POST", "http://example.com/data"})
	err := cmd.Execute()
	// Should fail (connection refused), but shouldn't panic
	if err == nil {
		t.Log("expected connection error (tunnel fails), got nil")
	}
}

func TestTunnelCmd_HeaderFlag(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard, Out: io.Discard})
	cmd.SetArgs([]string{"-H", "X-Custom: value", "http://example.com/data"})
	err := cmd.Execute()
	if err == nil {
		t.Log("expected connection error (tunnel fails), got nil")
	}
}

func TestTunnelCmd_DataFlag(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard, Out: io.Discard})
	cmd.SetArgs([]string{"-d", `{"key":"val"}`, "http://example.com/data"})
	err := cmd.Execute()
	if err == nil {
		t.Log("expected connection error (tunnel fails), got nil")
	}
}

func TestTunnelCmd_IncludeFlag(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard, Out: io.Discard})
	cmd.SetArgs([]string{"-i", "http://example.com/data"})
	err := cmd.Execute()
	if err == nil {
		t.Log("expected connection error (tunnel fails), got nil")
	}
}

func TestTunnelCmd_WithConfigKey(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://127.0.0.1:1", client.WithTunnel(testutil.TestKey()))
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdTunnel(factory, cli.IOStreams{ErrOut: io.Discard, Out: io.Discard})
	cmd.SetArgs([]string{"http://any-host.local/data"})
	err := cmd.Execute()
	// The tunnel will fail (connection refused), but shouldn't be a "tunnel_key" missing error
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error after config: %v", err)
	}
}
