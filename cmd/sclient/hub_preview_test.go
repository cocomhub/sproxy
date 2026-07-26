// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
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

func TestRelayRemoveNodeCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayRemoveNode(cli.IOStreams{}, nil)
	if cmd.Use != "remove-node <node-id>" {
		t.Fatalf("expected Use 'remove-node <node-id>', got %q", cmd.Use)
	}
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("remove-node should require exactly 1 arg")
	}
}

func TestRelayRemoveNodeCmd_Success(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes/test-node" && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	var buf strings.Builder
	cmd := NewCmdRelayRemoveNode(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", mock.URL)
	cmd.SetArgs([]string{"test-node"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("remove-node command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已移除节点") {
		t.Fatalf("expected success message, got: %s", buf.String())
	}
}

func TestRelayRemoveNodeCmd_NotFound(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	cmd := NewCmdRelayRemoveNode(cli.IOStreams{ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", mock.URL)
	cmd.SetArgs([]string{"nonexistent-node"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent node")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("expected '不存在' error, got: %v", err)
	}
}

func TestRelayStatsCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdRelayStats(cli.IOStreams{}, nil)
	if cmd.Use != "stats" {
		t.Fatalf("expected Use 'stats', got %q", cmd.Use)
	}
}

func TestRelayStatsCmd_Success(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/stats" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"node_count": 3})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	var buf strings.Builder
	cmd := NewCmdRelayStats(cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", mock.URL)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stats command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "3") {
		t.Fatalf("expected output to contain node count, got: %s", buf.String())
	}
}

func TestRelayStatsCmd_ServerError(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	cmd := NewCmdRelayStats(cli.IOStreams{ErrOut: io.Discard}, nil)
	cmd.Flags().Set("hub", mock.URL)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestPreviewCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdPreview(clientfactory.NewMock(nil, nil), cli.IOStreams{}, &state.State{}, nil)
	if cmd.Use != "preview <filename>" {
		t.Fatalf("expected Use 'preview <filename>', got %q", cmd.Use)
	}
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("preview should require exactly 1 arg")
	}
}

func TestPreviewCmd_TextFile(t *testing.T) {
	content := "line1\nline2\nline3\n"
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-File-Checksum", "abc123")
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", len(content)))
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(content))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	var buf strings.Builder
	cmd := NewCmdPreview(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st, nil)
	cmd.PersistentFlags().String("server", "", "server address")
	cmd.PersistentFlags().Set("server", mock.URL)

	cmd.SetArgs([]string{"test.txt"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preview command failed: %v", err)
	}

	if !strings.Contains(buf.String(), "line1") {
		t.Fatalf("expected output to contain file content, got: %s", buf.String())
	}
}

func TestPreviewCmd_ImageFile(t *testing.T) {

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/stat" {
			w.Header().Set("X-File-Checksum", "abc")
			w.Header().Set("X-File-Size", "100")
			w.Header().Set("X-File-IsDir", "false")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-File-Checksum", "abc")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake-image-data"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	st := &state.State{CurrentDir: ""}
	var buf strings.Builder
	cmd := NewCmdPreview(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard, In: strings.NewReader("\n")}, st, nil)
	cmd.PersistentFlags().String("server", "", "server address")
	cmd.PersistentFlags().Set("server", mock.URL)

	cmd.SetArgs([]string{"test.png"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preview command failed: %v", err)
	}

	if !strings.Contains(buf.String(), "正在打开图片预览") {
		t.Fatalf("expected image preview message, got: %s", buf.String())
	}
}

func TestPreviewCmd_UnknownExt(t *testing.T) {
	t.Parallel()
	// 验证 isTextExt 和 isImageExt 对未知扩展名返回 false
	ext := ".bin"
	if isImageExt(ext) || isTextExt(ext) {
		t.Fatal(".bin should not be a known extension")
	}
	ext = ".exe"
	if isImageExt(ext) || isTextExt(ext) {
		t.Fatal(".exe should not be a known extension")
	}
	ext = ".zip"
	if isImageExt(ext) || isTextExt(ext) {
		t.Fatal(".zip should not be a known extension")
	}
}
