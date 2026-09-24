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
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestNewCmdDu_Use(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDu(clientfactory.NewMock(nil, nil), cli.IOStreams{}, &state.State{})
	if cmd.Use != "du [path]" {
		t.Errorf("Use = %q, want du [path]", cmd.Use)
	}
}

func TestNewCmdDu_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/du" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("path") != "dir/sub" {
			t.Errorf("path = %q, want dir/sub", r.URL.Query().Get("path"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"path": "user/dir/sub", "dirs": 3, "files": 7, "size": 12345},
		})
	}))
	defer ts.Close()

	svc := newTestClient(t, ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdDu(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{CurrentDir: ""})
	cmd.SetArgs([]string{"dir/sub"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("du failed: %v", err)
	}
	if !strings.Contains(buf.String(), "7 文件") || !strings.Contains(buf.String(), "3 目录") {
		t.Errorf("du output 缺统计：%s", buf.String())
	}
}

func TestNewCmdDF_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"disk_usage":{"storage_root":"./storage","total_files":10,"total_size":1024},
			"request_counts":{"total":1,"2xx":1,"4xx":0,"5xx":0},
			"disk_total":100000000000,"disk_free":50000000000,"disk_used":50000000000,
			"quota":{"usage":1048576,"max_bytes":1073741824,"watermark":0}
		}`))
	}))
	defer ts.Close()

	svc := newTestClient(t, ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdDF(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("df failed: %v", err)
	}
	if !strings.Contains(buf.String(), "磁盘") || !strings.Contains(buf.String(), "配额") {
		t.Errorf("df output 缺类型行：%s", buf.String())
	}
}

// newTestClient 构造指向 httptest.Server 的 FileClient（独立连接池，禁共享客户端）。
func newTestClient(t *testing.T, serverURL string) *client.FileClient {
	t.Helper()
	return client.NewFileClient(serverURL)
}
