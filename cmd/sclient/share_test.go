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
)

func TestShareCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := NewCmdShare(clientfactory.NewMock(nil, nil), cli.IOStreams{})
	if cmd.Use != "share" {
		t.Errorf("expected Use=share, got %s", cmd.Use)
	}
	if cmd.Short != "文件分享管理" {
		t.Errorf("expected Short=文件分享管理, got %s", cmd.Short)
	}
}

func TestShareCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmd := NewCmdShare(clientfactory.NewMock(nil, nil), cli.IOStreams{})
	cmds := cmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	for _, name := range []string{"create", "list", "revoke"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestShareCreateCmd_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdShareCreate(clientfactory.NewMock(nil, nil), cli.IOStreams{})
	f := cmd.Flags()
	ttl, err := f.GetString("ttl")
	if err != nil || ttl != "24h" {
		t.Errorf("expected --ttl default 24h, got %v", ttl)
	}
	maxDL, _ := f.GetInt("max-downloads")
	if maxDL != 0 {
		t.Errorf("expected --max-downloads default 0, got %d", maxDL)
	}
	oneTime, _ := f.GetBool("one-time")
	if oneTime {
		t.Errorf("expected --one-time default false")
	}
}

func TestShareCmd_Integration(t *testing.T) {
	// 不使用 t.Parallel() — 与 cmd_test.go 中其他集成测试保持一致

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/share":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"token":"test123","filename":"file.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false}`))
		case "/api/shares":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"shares":[{"token":"test123","filename":"file.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false,"expired":false}]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)

	// 测试 share create
	var buf strings.Builder
	cmd := NewCmdShareCreate(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"file.txt", "--ttl", "24h"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("share create failed: %v", err)
	}
	if !strings.Contains(buf.String(), "test123") {
		t.Errorf("expected output to contain token, got: %s", buf.String())
	}

	// 测试 share list
	var buf2 strings.Builder
	cmd2 := NewCmdShareList(factory, cli.IOStreams{Out: &buf2, ErrOut: io.Discard})
	cmd2.SetArgs(nil)
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("share list failed: %v", err)
	}
	if !strings.Contains(buf2.String(), "file.txt") {
		t.Errorf("expected output to contain filename, got: %s", buf2.String())
	}
}

func TestShareCmd_Revoke(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && r.URL.Path == "/api/shares/test123" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"message":"分享链接已撤销"}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)

	var buf strings.Builder
	cmd := NewCmdShareRevoke(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"test123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("share revoke failed: %v", err)
	}

	if !strings.Contains(buf.String(), "test123") {
		t.Errorf("expected output to contain token, got: %s", buf.String())
	}
}

func TestShareCreateCmd_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/share" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdShareCreate(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.PersistentFlags().String("server", "", "")
	cmd.Flags().Set("ttl", "24h")
	cmd.SetArgs([]string{"test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}
