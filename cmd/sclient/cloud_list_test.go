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
	"github.com/spf13/cobra"
)

func TestCloudListCmd_UseAndArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{}, nil)
	if cmd.Use != "list" {
		t.Fatalf("expected Use 'list', got %q", cmd.Use)
	}
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

func TestCloudListCmd_ListTasks(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/tasks" && r.Method == http.MethodGet {
			tasks := []cloudTaskInfo{
				{ID: "task-1", URL: "https://example.com/a.zip", Filename: "a.zip", Status: "completed", TotalSize: 1000},
				{ID: "task-2", URL: "https://example.com/b.zip", Filename: "b.zip", Status: "downloading", TotalSize: 5000, Downloaded: 2000},
				{ID: "task-3", URL: "https://example.com/c.zip", Filename: "c.zip", Status: "pending", TotalSize: 2000},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"tasks": tasks})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")

	var buf strings.Builder
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)

	root.SetArgs([]string{"list", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}

	if !strings.Contains(buf.String(), "task-1") {
		t.Fatalf("expected output to contain task-1, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "task-2") {
		t.Fatalf("expected output to contain task-2, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "task-3") {
		t.Fatalf("expected output to contain task-3, got: %s", buf.String())
	}
}

func TestCloudListCmd_EmptyList(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tasks": []cloudTaskInfo{}})
	}))
	defer mock.Close()

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")

	var buf strings.Builder
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)

	root.SetArgs([]string{"list", "--server", mock.URL})
	if err := root.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}

	if !strings.Contains(buf.String(), "暂无云端下载任务") {
		t.Fatalf("expected '暂无云端下载任务', got: %q", buf.String())
	}
}

func TestCloudListCmd_JSONOutput(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tasks": []cloudTaskInfo{
			{ID: "task-1", URL: "https://example.com/f.zip", Filename: "f.zip", Status: "completed"},
		}})
	}))
	defer mock.Close()

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	root.PersistentFlags().Bool("json", false, "")

	var buf strings.Builder
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)

	root.SetArgs([]string{"list", "--server", mock.URL, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}

	if !strings.Contains(buf.String(), `"task-1"`) {
		t.Fatalf("expected JSON output with task-1, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"tasks"`) {
		t.Fatalf("expected JSON output with tasks key, got: %s", buf.String())
	}
}

func TestCloudListCmd_StatusFilter(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		if status != "completed" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "expected status=completed"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tasks": []cloudTaskInfo{
			{ID: "task-completed", Status: "completed"},
		}})
	}))
	defer mock.Close()

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")

	var buf strings.Builder
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)

	root.SetArgs([]string{"list", "--server", mock.URL, "--status", "completed"})
	if err := root.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}

	if !strings.Contains(buf.String(), "task-completed") {
		t.Fatalf("expected output to contain task-completed, got: %s", buf.String())
	}
}

func TestCloudListCmd_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer mock.Close()

	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")

	var buf strings.Builder
	cmd := NewCmdCloudList(clientfactory.NewMock(nil, nil), cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)

	root.SetArgs([]string{"list", "--server", mock.URL})
	err := root.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}
