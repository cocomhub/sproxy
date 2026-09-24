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
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

// ---- volume 子命令（用户自有卷管理）----

// volumeMockServer 模拟服务端用户卷 API（POST/GET/DELETE /api/volumes/user）。
func volumeMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/volumes/user":
			_, _ = io.WriteString(w, `{"success":true}`)
		case "GET /api/volumes/user":
			_, _ = io.WriteString(w, `{"volumes":[{"name":"disk1","type":"baidupcs","capacity":107374182400,"extra":{"bduss":"x"}}]}`)
		case "DELETE /api/volumes/user":
			_, _ = io.WriteString(w, `{"success":true}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestVolumeCreate_TextOutput(t *testing.T) {
	t.Parallel()
	mock := volumeMockServer(t)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolume(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"create", "disk1", "--type", "baidupcs", "--extra", `{"bduss":"test"}`})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume create failed: %v", err)
	}
	if !strings.Contains(out.String(), "success") && !strings.Contains(out.String(), "创建成功") {
		t.Errorf("输出应包含成功提示:\n%s", out.String())
	}
}

func TestVolumeList_TextOutput(t *testing.T) {
	t.Parallel()
	mock := volumeMockServer(t)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolume(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume list failed: %v", err)
	}
	for _, want := range []string{"disk1", "baidupcs", "100"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出缺少 %q:\n%s", want, out.String())
		}
	}
}

func TestVolumeList_JSON(t *testing.T) {
	t.Parallel()
	mock := volumeMockServer(t)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolume(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"list", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume list --json failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out.String()), &parsed); err != nil {
		t.Fatalf("输出应为合法 JSON: %v\n%s", err, out.String())
	}
}

func TestVolumeDelete(t *testing.T) {
	t.Parallel()
	mock := volumeMockServer(t)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolume(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"delete", "disk1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("volume delete failed: %v", err)
	}
	if !strings.Contains(out.String(), "success") && !strings.Contains(out.String(), "删除成功") {
		t.Errorf("输出应包含成功提示:\n%s", out.String())
	}
}

func TestVolumeCreate_BadExtra(t *testing.T) {
	t.Parallel()
	mock := volumeMockServer(t)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out strings.Builder
	cmd := NewCmdVolume(factory, cli.IOStreams{Out: &out, ErrOut: io.Discard}, &state.State{})
	cmd.SetArgs([]string{"create", "disk1", "--type", "baidupcs", "--extra", "not-json"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("非法 --extra 应报错")
	}
	if !strings.Contains(err.Error(), "extra") && !strings.Contains(err.Error(), "JSON") {
		t.Errorf("错误信息应提及 extra/JSON: %v", err)
	}
}
