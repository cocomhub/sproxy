// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
)

func TestRunHubStatusWithIO(t *testing.T) {
	t.Parallel()

	t.Run("with_nodes", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/hub/nodes" {
				t.Errorf("expected /api/hub/nodes, got %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"id": "node1", "addr": "10.0.0.1:8080", "connected": "2026-01-01T00:00:00Z"},
				{"id": "node2"},
			})
		}))
		defer mock.Close()

		hubAddr := "ws" + mock.URL[4:] + "/ws"
		var buf strings.Builder
		err := runHubStatusWithIO(context.Background(), hubAddr, &buf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		output := buf.String()
		if !strings.Contains(output, "在线节点数量: 2") {
			t.Errorf("expected '在线节点数量: 2', got %s", output)
		}
		if !strings.Contains(output, "node1") {
			t.Errorf("expected 'node1', got %s", output)
		}
		if !strings.Contains(output, "10.0.0.1:8080") {
			t.Errorf("expected '10.0.0.1:8080', got %s", output)
		}
	})

	t.Run("empty_nodes", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("[]"))
		}))
		defer mock.Close()

		hubAddr := "ws" + mock.URL[4:] + "/ws"
		var buf strings.Builder
		err := runHubStatusWithIO(context.Background(), hubAddr, &buf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		output := buf.String()
		if !strings.Contains(output, "在线节点数量: 0") {
			t.Errorf("expected '在线节点数量: 0', got %s", output)
		}
	})

	t.Run("http_error", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer mock.Close()

		hubAddr := "ws" + mock.URL[4:] + "/ws"
		var buf strings.Builder
		err := runHubStatusWithIO(context.Background(), hubAddr, &buf)
		if err == nil {
			t.Fatal("expected error for HTTP 500")
		}
		if !strings.Contains(err.Error(), "hub 返回错误状态") {
			t.Errorf("expected error message about hub status, got %v", err)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json"))
		}))
		defer mock.Close()

		hubAddr := "ws" + mock.URL[4:] + "/ws"
		var buf strings.Builder
		err := runHubStatusWithIO(context.Background(), hubAddr, &buf)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
		if !strings.Contains(err.Error(), "解析响应失败") {
			t.Errorf("expected error message about parse failure, got %v", err)
		}
	})
}

func TestNewCmdDiag_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDiag(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if cmd.Use != "diag" {
		t.Errorf("expected Use 'diag', got %q", cmd.Use)
	}
	for _, name := range []string{"ping", "hub-status"} {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("missing flag: %s", name)
		}
	}
}

func TestNewCmdDiag_HelpOnNoFlag(t *testing.T) {
	cmd := NewCmdDiag(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	// cmd.Help() 返回 nil，所以 err 应为 nil
	if err != nil {
		t.Errorf("expected no error when no flags provided, got: %v", err)
	}
}
