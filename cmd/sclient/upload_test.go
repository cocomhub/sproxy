// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestNewCmdUpload_HappyPath(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "test.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" {
			t.Errorf("expected path /upload, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"success":true,"message":"uploaded","file_checksum":"abc123"}`))
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, st)

	cmd.SetArgs([]string{srcFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upload command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "成功") {
		t.Errorf("expected success message, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "abc123") {
		t.Errorf("expected checksum in output, got: %s", buf.String())
	}
}

func TestNewCmdUpload_ServerError(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "test.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf, errBuf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: &buf, ErrOut: &errBuf}, st)

	cmd.SetArgs([]string{srcFile})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when server returns 500")
	}
}

func TestNewCmdUpload_InitClientError(t *testing.T) {
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "test.txt")
	if err := os.WriteFile(srcFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	factory := clientfactory.NewMock(nil, io.ErrClosedPipe)
	var buf, errBuf strings.Builder
	st := &state.State{CurrentDir: ""}
	cmd := NewCmdUpload(factory, cli.IOStreams{Out: &buf, ErrOut: &errBuf}, st)

	cmd.SetArgs([]string{srcFile})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when factory returns error")
	}
}
