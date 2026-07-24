// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestRelayCmd_Usage(t *testing.T) {
	t.Parallel()
	if relayCmd.Use != "relay" {
		t.Errorf("expected Use=relay, got %s", relayCmd.Use)
	}
	if relayCmd.Short != "中继节点管理" {
		t.Errorf("expected Short=中继节点管理, got %s", relayCmd.Short)
	}
}

func TestRelayCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmds := relayCmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	for _, name := range []string{"start", "status", "stop"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestRelayStatusCmd_Integration(t *testing.T) {
	defer captureRootCmdArgs()()

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

	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "relay", "status"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "node-1") {
		t.Errorf("expected output to contain node-1, got: %s", output)
	}
}

func TestRelayStatusCmd_Empty(t *testing.T) {
	defer captureRootCmdArgs()()

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

	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "relay", "status"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "暂无已连接节点") {
		t.Errorf("expected empty message, got: %s", output)
	}
}

func TestRelayStopCmd(t *testing.T) {
	t.Parallel()
	output := testutil.CaptureStdout(func() {
		relayStopCmd.RunE(relayStopCmd, nil)
	})

	if !strings.Contains(output, "SIGINT") {
		t.Errorf("expected output to contain SIGINT, got: %s", output)
	}
}
