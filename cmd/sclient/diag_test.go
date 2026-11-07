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

	t.Run("with_nodes", testRunHubStatusWithNodes)
	t.Run("empty_nodes", testRunHubStatusEmptyNodes)
	t.Run("http_error", testRunHubStatusHTTPError)
	t.Run("invalid_json", testRunHubStatusInvalidJSON)
}

// newHubStatusServer 起一个返回节点列表的 mock hub，返回其 WS 地址（t.Cleanup 关闭）。
func newHubStatusServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	mock := httptest.NewServer(handler)
	t.Cleanup(mock.Close)
	return "ws" + mock.URL[4:] + "/ws"
}

// hubStatusRun 调用 runHubStatusWithIO，错误即 Fatal，返回输出文本。
func hubStatusRun(t *testing.T, hubAddr string) string {
	t.Helper()
	var buf strings.Builder
	if err := runHubStatusWithIO(context.Background(), hubAddr, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return buf.String()
}

// hubStatusRunErr 调用 runHubStatusWithIO，仅返回错误（不 Fatal）。
func hubStatusRunErr(t *testing.T, hubAddr string) error {
	t.Helper()
	var buf strings.Builder
	return runHubStatusWithIO(context.Background(), hubAddr, &buf)
}

// assertContainsAll 断言 output 同时包含全部子串。
func assertContainsAll(t *testing.T, output string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(output, s) {
			t.Errorf("expected %q in output, got %s", s, output)
		}
	}
}

func testRunHubStatusWithNodes(t *testing.T) {
	hubAddr := newHubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			t.Errorf("expected /api/hub/nodes, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "node1", "addr": "10.0.0.1:8080", "connected": "2026-01-01T00:00:00Z"},
			{"id": "node2"},
		})
	})
	output := hubStatusRun(t, hubAddr)
	assertContainsAll(t, output, "在线节点数量: 2", "node1", "10.0.0.1:8080")
}

func testRunHubStatusEmptyNodes(t *testing.T) {
	hubAddr := newHubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	})
	output := hubStatusRun(t, hubAddr)
	assertContainsAll(t, output, "在线节点数量: 0")
}

func testRunHubStatusHTTPError(t *testing.T) {
	hubAddr := newHubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	err := hubStatusRunErr(t, hubAddr)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "hub 返回错误状态") {
		t.Errorf("expected error message about hub status, got %v", err)
	}
}

func testRunHubStatusInvalidJSON(t *testing.T) {
	hubAddr := newHubStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})
	err := hubStatusRunErr(t, hubAddr)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "解析响应失败") {
		t.Errorf("expected error message about parse failure, got %v", err)
	}
}

func TestNewCmdDiag_Flags(t *testing.T) {
	t.Parallel()
	cmd := NewCmdDiag(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if cmd.Use != "diag" {
		t.Errorf("expected Use 'diag', got %q", cmd.Use)
	}
	for _, name := range []string{"ping", "hub-status"} {
		if cmd.Flags().Lookup(name) == nil {
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
