// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestRelayRemoveNodeCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	if relayRemoveNodeCmd.Use != "remove-node <node-id>" {
		t.Fatalf("expected Use 'remove-node <node-id>', got %q", relayRemoveNodeCmd.Use)
	}
	if relayRemoveNodeCmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := relayRemoveNodeCmd.Args(relayRemoveNodeCmd, []string{}); err == nil {
		t.Error("remove-node should require exactly 1 arg")
	}
}

func TestRelayRemoveNodeCmd_Success(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/nodes/test-node" && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"relay", "remove-node", "--server", mock.URL, "test-node"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "已移除节点") {
		t.Fatalf("expected success message, got: %s", out)
	}
}

func TestRelayRemoveNodeCmd_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"relay", "remove-node", "--server", mock.URL, "nonexistent-node"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent node")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("expected '不存在' error, got: %v", err)
	}
}

func TestRelayStatsCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	if relayStatsCmd.Use != "stats" {
		t.Fatalf("expected Use 'stats', got %q", relayStatsCmd.Use)
	}
}

func TestRelayStatsCmd_Success(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hub/stats" && r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"node_count": 3})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"relay", "stats", "--server", mock.URL})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "3") {
		t.Fatalf("expected output to contain node count, got: %s", out)
	}
}

func TestRelayStatsCmd_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"relay", "stats", "--server", mock.URL})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestPreviewCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	if previewCmd.Use != "preview <filename>" {
		t.Fatalf("expected Use 'preview <filename>', got %q", previewCmd.Use)
	}
	if previewCmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := previewCmd.Args(previewCmd, []string{}); err == nil {
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

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"preview", "--server", mock.URL, "test.txt"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "line1") {
		t.Fatalf("expected output to contain file content, got: %s", out)
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

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"preview", "--server", mock.URL, "test.png"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "正在打开图片预览") {
		t.Fatalf("expected image preview message, got: %s", out)
	}
}

func TestPreviewCmd_UnknownExt(t *testing.T) {
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
