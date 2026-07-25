// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestStatsCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := NewCmdStats(clientfactory.NewMock(nil, nil), cli.IOStreams{})
	if cmd.Use != "stats" {
		t.Errorf("expected Use=stats, got %s", cmd.Use)
	}
	if cmd.Short != "显示服务器统计信息" {
		t.Errorf("expected Short=显示服务器统计信息, got %s", cmd.Short)
	}
}

func TestNewCmdStats_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"disk_usage":{"uploads_dir":"./uploads","total_files":10,"total_size":1024},
			"request_counts":{"total":100,"2xx":80,"4xx":15,"5xx":5},
			"active_connections":3,
			"files_uploaded":5,"files_downloaded":20,"files_deleted":2,
			"bytes_uploaded":50000,"bytes_downloaded":200000,
			"max_storage_bytes":1073741824,"storage_usage":1048576,
			"disk_total":100000000000,"disk_free":50000000000,"disk_used":50000000000
		}`))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdStats(factory, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stats command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "10") {
		t.Errorf("expected output to contain file count, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "100") {
		t.Errorf("expected output to contain request count, got: %s", buf.String())
	}
}

func TestNewCmdConfigRemote_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"log_level":"info","log_format":"text",
			"auth_token_set":true,"tunnel_key_set":false,
			"rate_limit_requests":10,"rate_limit_window":"1s",
			"max_storage_bytes":0,"chunk_size":4194304,
			"upload_session_ttl":"24h0m0s",
			"versioning_enabled":false,"versioning_max_versions":0,
			"cloud_max_concurrent":3,"cloud_sync_threshold":20971520,
			"hub_enabled":false,"tls_enabled":false,
			"addr":":18083","uploads_dir":"./uploads"
		}`))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdConfigRemote(factory, cli.IOStreams{Out: &buf})

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config remote command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "info") {
		t.Errorf("expected output to contain log_level, got: %s", buf.String())
	}
}

func TestNewCmdConfigRemoteSet_Integration(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/api/config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"changed":true}`))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdConfigRemoteSet(factory, cli.IOStreams{Out: &buf})

	cmd.SetArgs([]string{"log_level", "debug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("config remote set failed: %v", err)
	}
	if !strings.Contains(buf.String(), "debug") {
		t.Errorf("expected output to contain 'debug', got: %s", buf.String())
	}
}
