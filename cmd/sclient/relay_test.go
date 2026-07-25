// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestRelayCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelay(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Use != "relay" {
		t.Errorf("expected Use=relay, got %s", cmd.Use)
	}
	if cmd.Short != "中继节点管理" {
		t.Errorf("expected Short=中继节点管理, got %s", cmd.Short)
	}
}

func TestRelayCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelay(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	for _, name := range []string{"start", "status", "stop", "remove-node", "stats"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestRelayStartCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStart(cli.IOStreams{Out: io.Discard})
	if cmd.Use != "start" {
		t.Errorf("expected Use 'start', got %q", cmd.Use)
	}
	if cmd.Short != "启动中继节点，连接到 Hub" {
		t.Errorf("expected Short '启动中继节点，连接到 Hub', got %q", cmd.Short)
	}
	for _, name := range []string{"hub", "local", "node-id"} {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("missing flag: %s", name)
		}
	}
}

func TestRelayStopCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStop(cli.IOStreams{Out: io.Discard})
	if cmd.Use != "stop" {
		t.Errorf("expected Use 'stop', got %q", cmd.Use)
	}
	if cmd.Short != "停止中继节点" {
		t.Errorf("expected Short '停止中继节点', got %q", cmd.Short)
	}
}

func TestRelayStatsCmd_Integration(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/stats" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"node_count":3}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStats(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stats command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "3") {
		t.Errorf("expected output to contain node count, got: %s", buf.String())
	}
}

func TestRelayStatusCmd_Integration(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":"node-1","addr":"192.168.1.1:54321","connected":"2026-07-24T10:30:00+08:00"}]`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStatus(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "node-1") {
		t.Errorf("expected output to contain node-1, got: %s", buf.String())
	}
}

func TestRelayStatusCmd_Empty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStatus(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.Flags().Set("hub", ts.URL)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "暂无已连接节点") {
		t.Errorf("expected empty message, got: %s", buf.String())
	}
}

func TestRelayStopCmd(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	cmd := NewCmdRelayStop(cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "SIGINT") {
		t.Errorf("expected output to contain SIGINT, got: %s", buf.String())
	}
}
