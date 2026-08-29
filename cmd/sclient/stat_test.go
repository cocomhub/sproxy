// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// ---- stat command: 无参显示本地 client 状态 ----

func TestStatCmd_NoArg_LocalStatus(t *testing.T) {
	var buf strings.Builder
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	cmd := NewCmdStat(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, cfgSvc)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stat (no arg) failed: %v", err)
	}
	if !strings.Contains(buf.String(), "server_url") {
		t.Errorf("expected output to contain 'server_url', got: %q", buf.String())
	}
	if !strings.Contains(buf.String(), Version) {
		t.Errorf("expected output to contain version %q, got: %q", Version, buf.String())
	}
}

func TestStatCmd_AllowNoArg(t *testing.T) {
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	cmd := NewCmdStat(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, cfgSvc)
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Errorf("stat should accept no args, got error: %v", err)
	}
	if err := cmd.Args(cmd, []string{"server"}); err != nil {
		t.Errorf("stat should accept 'server' arg, got error: %v", err)
	}
}

// ---- stat server: 显示远端服务状态 ----

func TestStatCmd_Server(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"files_uploaded": 3})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	root := &cobra.Command{Use: "sclient"}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("access-key", "", "")
	root.PersistentFlags().String("access-key-secret", "", "")
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(NewCmdStat(factory, ios, cfgSvc))
	cmd, _, err := root.Find([]string{"stat", "server"}) // 先解析子命令再执行，确保走真实 dispatch
	if err != nil {
		t.Fatalf("find stat server failed: %v", err)
	}
	root.SetArgs([]string{"stat", "server", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("stat server failed: %v", err)
	}
	if cmd.Name() != "server" {
		t.Fatalf("expected dispatched subcommand 'server', got %q", cmd.Name())
	}
	if !strings.Contains(buf.String(), "3") {
		t.Errorf("expected output to contain stats content '3', got: %q", buf.String())
	}
}
