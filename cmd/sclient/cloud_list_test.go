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
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/tasks" && r.Method == http.MethodGet {
			tasks := []map[string]any{
				{"id": "task-1", "url": "https://example.com/a.zip", "filename": "a.zip", "status": "completed", "total_size": 1000},
				{"id": "task-2", "url": "https://example.com/b.zip", "filename": "b.zip", "status": "downloading", "total_size": 5000, "downloaded": 2000},
				{"id": "task-3", "url": "https://example.com/c.zip", "filename": "c.zip", "status": "pending", "total_size": 2000},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"tasks": tasks, "total": len(tasks)})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudList(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
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
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tasks": []cloudTaskInfo{}, "total": 0})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudList(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "暂无云端下载任务") {
		t.Fatalf("expected '暂无云端下载任务', got: %q", buf.String())
	}
}

func TestCloudListCmd_JSONOutput(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tasks": []cloudTaskInfo{
			{ID: "task-1", URL: "https://example.com/f.zip", Filename: "f.zip", Status: "completed"},
		}, "total": 1})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	cmd := NewCmdCloudList(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	root.AddCommand(cmd)
	root.SetArgs([]string{"list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "task-1") {
		t.Fatalf("expected output with task-1, got: %s", buf.String())
	}
}

func TestCloudListCmd_StatusFilter(t *testing.T) {
	t.Parallel()
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
		}, "total": 1})
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudList(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"--status", "completed"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !strings.Contains(buf.String(), "task-completed") {
		t.Fatalf("expected output to contain task-completed, got: %s", buf.String())
	}
}

func TestCloudListCmd_ServerError(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudList(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

func TestGetCloudServerURL_FromFlag(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	root.PersistentFlags().Set("server", "http://test-server:18083")
	root.PersistentFlags().Set("auth-token", "test-token")

	cmd := &cobra.Command{Use: "test"}
	root.AddCommand(cmd)

	serverURL, authToken := getCloudServerURL(cmd, nil)
	if serverURL != "http://test-server:18083" {
		t.Errorf("expected server URL from flag, got %q", serverURL)
	}
	if authToken != "test-token" {
		t.Errorf("expected auth token from flag, got %q", authToken)
	}
}

func TestGetCloudServerURL_FromConfig(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")

	cmd := &cobra.Command{Use: "test"}
	root.AddCommand(cmd)

	cfgSvc := &testConfigProvider{cfg: &client.Config{ServerURL: "http://cfg-server:18083", AuthToken: "cfg-token"}}
	serverURL, authToken := getCloudServerURL(cmd, cfgSvc)
	if serverURL != "http://cfg-server:18083" {
		t.Errorf("expected server URL from config, got %q", serverURL)
	}
	if authToken != "cfg-token" {
		t.Errorf("expected auth token from config, got %q", authToken)
	}
}

func TestGetCloudServerURL_FlagOverridesConfig(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{}
	root.PersistentFlags().String("server", "", "")
	root.PersistentFlags().String("auth-token", "", "")
	root.PersistentFlags().Set("server", "http://flag-server:18083")
	root.PersistentFlags().Set("auth-token", "flag-token")

	cmd := &cobra.Command{Use: "test"}
	root.AddCommand(cmd)

	cfgSvc := &testConfigProvider{cfg: &client.Config{ServerURL: "http://cfg-server:18083", AuthToken: "cfg-token"}}
	serverURL, authToken := getCloudServerURL(cmd, cfgSvc)
	if serverURL != "http://flag-server:18083" {
		t.Errorf("expected flag to override config, got %q", serverURL)
	}
	if authToken != "flag-token" {
		t.Errorf("expected flag to override config token, got %q", authToken)
	}
}
