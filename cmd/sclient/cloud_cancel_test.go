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

func TestCloudCancelCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdCloudCancel(nil, cli.IOStreams{}, nil)
	if cmd.Use != "cancel <task-id>" {
		t.Fatalf("expected Use 'cancel <task-id>', got %q", cmd.Use)
	}
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Error("cancel should require exactly 1 arg")
	}
	if err := cmd.Args(cmd, []string{"task-1"}); err != nil {
		t.Errorf("cancel with 1 arg should be ok: %v", err)
	}
}

func TestCloudCancelCmd_Success(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/tasks/task-1/cancel" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "cancelled"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--server", mock.URL, "task-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Fatalf("expected success message, got: %s", buf.String())
	}
}

func TestCloudCancelCmd_NotFound(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--server", mock.URL, "nonexistent-task"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "任务不存在") {
		t.Fatalf("expected '任务不存在', got: %s", buf.String())
	}
}

func TestCloudCancelCmd_AlreadyCompleted(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "task already completed"})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--server", mock.URL, "completed-task"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "失败") {
		t.Fatalf("expected failure message, got: %s", buf.String())
	}
}

func TestCloudCancelCmd_JSONOutput(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "cancelled"})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--server", mock.URL, "--json", "task-json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), `"success"`) && !strings.Contains(buf.String(), `"task_id"`) {
		t.Fatalf("expected JSON output, got: %s", buf.String())
	}
}

func TestCloudCancelCmd_ServerError(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.PersistentFlags().String("server", "", "")
	cmd.PersistentFlags().String("auth-token", "", "")
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--server", mock.URL, "error-task"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected error to mention HTTP 500, got: %v", err)
	}
}
