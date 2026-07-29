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

func TestVersionCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &testConfigProvider{})
	if !strings.HasPrefix(cmd.Use, "version") {
		t.Errorf("expected Use to start with 'version', got %q", cmd.Use)
	}
}

func TestVersionCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard}, &testConfigProvider{})
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	// 注意："version" 命令既是"显示程序版本"又是"文件版本管理"的入口。
	// 这里的子命令是文件版本管理（list/restore/delete），而非程序版本子命令。
	for _, name := range []string{"list", "restore", "delete"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestVersionCmd_ShowVersion(t *testing.T) {
	Version = "1.2.3"
	BuildAt = "2026-07-26T12:00:00Z"
	t.Cleanup(func() {
		Version = "dev"
		BuildAt = "unknown"
	})

	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	var buf strings.Builder
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, cfgSvc)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "1.2.3") {
		t.Errorf("expected output to contain version '1.2.3', got: %s", output)
	}
	if !strings.Contains(output, "2026-07-26T12:00:00Z") {
		t.Errorf("expected output to contain build time, got: %s", output)
	}
}

func TestVersionCmd_NilConfigSvc(t *testing.T) {
	Version = "1.0.0"
	BuildAt = "test-build"
	t.Cleanup(func() {
		Version = "dev"
		BuildAt = "unknown"
	})

	// 使用返回空配置的 testConfigProvider，而非 nil 指针
	cfgSvc := &testConfigProvider{cfg: client.DefaultConfig()}
	var buf strings.Builder
	cmd := NewCmdVersion(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, cfgSvc)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("expected output to contain version, got: %s", output)
	}
}

func TestVersionRestoreCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersionRestore(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Use != "restore <filename> <version_id>" {
		t.Errorf("expected Use 'restore <filename> <version_id>', got %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("expected error for 0 args")
	}
	if err := cmd.Args(cmd, []string{"f.txt"}); err == nil {
		t.Error("expected error for 1 arg")
	}
	if err := cmd.Args(cmd, []string{"f.txt", "v1"}); err != nil {
		t.Errorf("expected no error for 2 args, got: %v", err)
	}
}

func TestVersionDeleteCmd_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdVersionDelete(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: io.Discard})
	if cmd.Use != "delete <filename> <version_id>" {
		t.Errorf("expected Use 'delete <filename> <version_id>', got %q", cmd.Use)
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("expected error for 0 args")
	}
	if err := cmd.Args(cmd, []string{"f.txt", "v1"}); err != nil {
		t.Errorf("expected no error for 2 args, got: %v", err)
	}
}

func TestVersionRestoreCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/versions/restore" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "restored"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	cmd := NewCmdVersionRestore(factory, ios)
	root.AddCommand(cmd)

	root.SetArgs([]string{"restore", "test.txt", "1", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("version restore failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已恢复文件") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
}

func TestVersionDeleteCmd_Integration(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/versions" && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "deleted"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	cmd := NewCmdVersionDelete(factory, ios)
	root.AddCommand(cmd)

	root.SetArgs([]string{"delete", "test.txt", "42", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("version delete failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已删除文件") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
}
