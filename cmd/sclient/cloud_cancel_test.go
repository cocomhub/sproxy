// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestCloudCancelCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	if cloudCancelCmd.Use != "cancel <task-id>" {
		t.Fatalf("expected Use 'cancel <task-id>', got %q", cloudCancelCmd.Use)
	}
	if cloudCancelCmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := cloudCancelCmd.Args(cloudCancelCmd, []string{}); err == nil {
		t.Error("cancel should require exactly 1 arg")
	}
	if err := cloudCancelCmd.Args(cloudCancelCmd, []string{"task-1"}); err != nil {
		t.Errorf("cancel with 1 arg should be ok: %v", err)
	}
}

func TestCloudCancelCmd_Success(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/tasks/task-1/cancel" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "cancelled"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "cancel", "--server", mock.URL, "task-1"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "已取消") {
		t.Fatalf("expected success message, got: %s", out)
	}
}

func TestCloudCancelCmd_NotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "cancel", "--server", mock.URL, "nonexistent-task"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "任务不存在") {
		t.Fatalf("expected '任务不存在', got: %s", out)
	}
}

func TestCloudCancelCmd_AlreadyCompleted(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "task already completed"})
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "cancel", "--server", mock.URL, "completed-task"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "失败") {
		t.Fatalf("expected failure message, got: %s", out)
	}
}

func TestCloudCancelCmd_JSONOutput(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "cancelled"})
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "cancel", "--server", mock.URL, "--json", "task-json"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, `"success"`) && !strings.Contains(out, `"task_id"`) {
		t.Fatalf("expected JSON output, got: %s", out)
	}
}

func TestCloudCancelCmd_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	// 确保 --json flag 被重置（防止前序测试污染）
	rootCmd.PersistentFlags().Set("json", "false")

	out := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"cloud-download", "cancel", "--server", mock.URL, "error-task"})
		rootCmd.Execute()
	})

	if !strings.Contains(out, "失败") && !strings.Contains(out, "HTTP 500") {
		t.Fatalf("expected failure message, got: %s", out)
	}
}
