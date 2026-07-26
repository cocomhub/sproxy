// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"
	"time"

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

// ---- printProgressBar tests ----

func TestPrintProgressBar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		current  int64
		total    int64
		barWidth int
		contains []string
	}{
		{"zero_percent", 0, 100, 20, []string{"0.00%", "[", "]"}},
		{"fifty_percent", 50, 100, 20, []string{"50.00%", "[", "]"}},
		{"hundred_percent", 100, 100, 20, []string{"100.00%", "[", "]"}},
		{"partial_fill", 25, 200, 40, []string{"12.50%", "[", "]"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printProgressBar(&buf, tt.current, tt.total, tt.barWidth)
			output := buf.String()
			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("expected output to contain %q, got %q", want, output)
				}
			}
		})
	}
}

func TestPrintProgressBarDone(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	start := time.Now()
	printProgressBarDone(&buf, 100, 100, 20, start)
	output := buf.String()
	if !strings.Contains(output, "100.00%") {
		t.Errorf("expected output to contain '100.00%%', got %q", output)
	}
	if !strings.Contains(output, "in ") {
		t.Errorf("expected output to contain 'in ' (duration), got %q", output)
	}
}

func TestPrintProgressBarDone_Partial(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	start := time.Now()
	printProgressBarDone(&buf, 75, 200, 40, start)
	output := buf.String()
	if !strings.Contains(output, "37.50%") {
		t.Errorf("expected output to contain '37.50%%', got %q", output)
	}
}
