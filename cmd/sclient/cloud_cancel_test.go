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
	cmd := NewCmdCloudCancel(clientfactory.NewMock(nil, nil), cli.IOStreams{}, nil)
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
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "cancelled"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"task-1"})
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
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"nonexistent-task"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel not found should not return error: %v", err)
	}
	if !strings.Contains(buf.String(), "404") && !strings.Contains(buf.String(), "not found") {
		t.Errorf("expected error message about 404/not found, got: %s", buf.String())
	}
}

func TestCloudCancelCmd_JSONOutput(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "cancelled"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"--json", "task-json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "task-json") {
		t.Errorf("expected output to contain task-json, got: %s", buf.String())
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
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"error-task"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server 500, got nil")
	}
}

func TestCloudCancelCmd_InvalidJSON(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not json"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"task-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel should not return error for invalid JSON: %v", err)
	}
	// CancelCloudTask 使用 doJSON 且 respBody=nil，200 OK 时不解析 body
	// 所以无效 JSON 不会导致错误，这是预期行为
}

func TestCloudCancelCmd_AlreadyCompleted(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": "task already completed"})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudCancel(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.Flags().Bool("json", false, "")
	cmd.SetArgs([]string{"completed-task"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when canceling completed task")
	}
}
