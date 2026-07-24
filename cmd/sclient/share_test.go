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

func TestShareCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := shareCmd
	if cmd.Use != "share" {
		t.Errorf("expected Use=share, got %s", cmd.Use)
	}
	if cmd.Short != "文件分享管理" {
		t.Errorf("expected Short=文件分享管理, got %s", cmd.Short)
	}
}

func TestShareCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmds := shareCmd.Commands()
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
	f := shareCreateCmd.Flags()
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
	defer captureRootCmdArgs()()

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

	// 测试 share create
	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "create", "file.txt", "--ttl", "24h"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "test123") {
		t.Errorf("expected output to contain token, got: %s", output)
	}

	// 测试 share list
	output2 := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "list"})
		_ = rootCmd.Execute()
	})
	if !strings.Contains(output2, "file.txt") {
		t.Errorf("expected output to contain filename, got: %s", output2)
	}
}

func TestShareCmd_Revoke(t *testing.T) {
	defer captureRootCmdArgs()()

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

	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "revoke", "test123"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "test123") {
		t.Errorf("expected output to contain token, got: %s", output)
	}
}
