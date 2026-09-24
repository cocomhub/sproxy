// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
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
	"github.com/spf13/cobra"
)

// syncConflictsMockCapture 捕获 mock 服务端收到的 conflicts 请求。
type syncConflictsMockCapture struct {
	path        string
	method      string
	body        map[string]string
	failSuccess bool
}

// newSyncConflictsMockServer 返回模拟 /api/sync/conflicts 端点的测试服务器。
// conflicts 为 list 返回的冲突条目（nil 时返回空列表）。
func newSyncConflictsMockServer(t *testing.T, conflicts []client.SyncConflictItem) (*httptest.Server, *syncConflictsMockCapture) {
	t.Helper()
	cap := &syncConflictsMockCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sync/conflicts", func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.method = r.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "conflicts": conflicts})
	})
	mux.HandleFunc("POST /api/sync/conflicts/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.method = r.Method
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
			return
		}
		cap.body = body
		w.Header().Set("Content-Type", "application/json")
		if cap.failSuccess {
			json.NewEncoder(w).Encode(map[string]any{"success": false})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "path": "dir/f.txt"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, cap
}

// TestSyncCmd_ConflictsSubcommandRegistered 验证 conflicts 子命令族注册。
func TestSyncCmd_ConflictsSubcommandRegistered(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{}, &state.State{}, nil)
	if sub := findSubCommand(cmd, "conflicts"); sub == nil {
		t.Fatal("expected conflicts subcommand registered under sync")
	}
}

// TestSyncConflictsCmd_List_Table 验证 list 表格输出包含冲突 ID 与 path。
func TestSyncConflictsCmd_List_Table(t *testing.T) {
	t.Parallel()
	mock, cap := newSyncConflictsMockServer(t, []client.SyncConflictItem{
		{ID: "cf-abc-1", Path: "dir/f.txt", HunkCount: 2, OursSHA: "aaaa1111", TheirsSHA: "bbbb2222", Timestamp: 1750000000},
	})
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync conflicts list failed: %v", err)
	}
	if cap.path != "/api/sync/conflicts" || cap.method != http.MethodGet {
		t.Fatalf("want GET /api/sync/conflicts, got %s %s", cap.method, cap.path)
	}
	out := buf.String()
	if !strings.Contains(out, "cf-abc-1") {
		t.Fatalf("expected conflict ID in table output, got: %s", out)
	}
	if !strings.Contains(out, "dir/f.txt") {
		t.Fatalf("expected conflict path in table output, got: %s", out)
	}
}

// TestSyncConflictsCmd_List_Empty 验证空列表输出"无未解决冲突"。
func TestSyncConflictsCmd_List_Empty(t *testing.T) {
	t.Parallel()
	mock, _ := newSyncConflictsMockServer(t, nil)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync conflicts list failed: %v", err)
	}
	if !strings.Contains(buf.String(), "无未解决冲突") {
		t.Fatalf("expected 无未解决冲突 in output, got: %s", buf.String())
	}
}

// TestSyncConflictsCmd_Resolve_Theirs 验证 resolve 调用 resolve 端点并输出已解决 + path。
func TestSyncConflictsCmd_Resolve_Theirs(t *testing.T) {
	t.Parallel()
	mock, cap := newSyncConflictsMockServer(t, nil)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"resolve", "cf-abc-1", "--strategy", "theirs"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync conflicts resolve failed: %v", err)
	}
	if cap.method != http.MethodPost || !strings.HasSuffix(cap.path, "/resolve") {
		t.Fatalf("want POST .../resolve, got %s %s", cap.method, cap.path)
	}
	if cap.body["choice"] != "theirs" {
		t.Fatalf("want choice theirs in body, got %+v", cap.body)
	}
	out := buf.String()
	if !strings.Contains(out, "已解决") || !strings.Contains(out, "cf-abc-1") || !strings.Contains(out, "theirs") {
		t.Fatalf("expected 已解决 + id + strategy in output, got: %s", out)
	}
}

// TestSyncConflictsCmd_Resolve_Manual_Content 验证 manual 策略把 --content 传给服务端。
func TestSyncConflictsCmd_Resolve_Manual_Content(t *testing.T) {
	t.Parallel()
	mock, cap := newSyncConflictsMockServer(t, nil)
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"resolve", "cf-abc-1", "--strategy", "manual", "--content", "merged text"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync conflicts resolve manual failed: %v", err)
	}
	if cap.body["choice"] != "manual" || cap.body["content"] != "merged text" {
		t.Fatalf("want choice manual + content in body, got %+v", cap.body)
	}
}

// TestSyncConflictsCmd_Resolve_MissingID 验证缺 id 时 cobra.ExactArgs 报错。
func TestSyncConflictsCmd_Resolve_MissingID(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"resolve", "--strategy", "ours"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when id missing")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Fatalf("expected cobra arg error, got: %v", err)
	}
}

// TestSyncConflictsCmd_Resolve_ManualWithoutContent 验证 manual 缺 --content 本地报错（不出网）。
func TestSyncConflictsCmd_Resolve_ManualWithoutContent(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"resolve", "cf-abc-1", "--strategy", "manual"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when manual strategy without --content")
	}
	if !strings.Contains(err.Error(), "--content") {
		t.Fatalf("expected error to mention --content, got: %v", err)
	}
}

// TestSyncConflictsCmd_Resolve_InvalidStrategy 验证非法 strategy 本地报错并列出可选值。
func TestSyncConflictsCmd_Resolve_InvalidStrategy(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"resolve", "cf-abc-1", "--strategy", "bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid strategy")
	}
	if !strings.Contains(err.Error(), "ours|theirs|manual") {
		t.Fatalf("expected error to list valid strategies, got: %v", err)
	}
}

// TestSyncConflictsCmd_List_JSON 验证 --json 输出 {conflicts: [...]}。
func TestSyncConflictsCmd_List_JSON(t *testing.T) {
	t.Parallel()
	mock, _ := newSyncConflictsMockServer(t, []client.SyncConflictItem{
		{ID: "cf-abc-1", Path: "dir/f.txt", HunkCount: 2, OursSHA: "aaaa1111", TheirsSHA: "bbbb2222", Timestamp: 1750000000},
	})
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := newCmdSyncConflicts(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	root.SetArgs([]string{"conflicts", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sync conflicts list --json failed: %v", err)
	}
	var out struct {
		Conflicts []client.SyncConflictItem `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("expected JSON output, got: %q (err=%v)", buf.String(), err)
	}
	if len(out.Conflicts) != 1 || out.Conflicts[0].ID != "cf-abc-1" {
		t.Fatalf("expected conflict in JSON, got: %+v", out.Conflicts)
	}
}

// TestSyncConflictsCmd_Resolve_SuccessFalse 服务端 success=false → 报错（变异探针）。
func TestSyncConflictsCmd_Resolve_SuccessFalse(t *testing.T) {
	t.Parallel()
	cap := &syncConflictsMockCapture{}
	var buf bytes.Buffer
	ios := cli.IOStreams{Out: &buf, ErrOut: io.Discard}
	cmd := newCmdSyncConflicts(&syncConflictsMockFactory{cap: cap}, ios)
	cap.failSuccess = true
	cmd.SetArgs([]string{"resolve", "cf-abc-1", "--strategy", "ours"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("success=false 应报错")
	}
}

// syncConflictsMockFactory 是 clientfactory.Factory 的测试替身（NewClient 返回 mock 客户端）。
type syncConflictsMockFactory struct {
	cap *syncConflictsMockCapture
}

func (f *syncConflictsMockFactory) NewClient(cmd *cobra.Command) (*client.FileClient, error) {
	return newMockClient(f.cap)
}

// newMockClient 构造指向 mock capture server 的 FileClient（带隔离传输）。
// 注意：httptest.Server 在测试内通过 t.Cleanup 关闭（newSyncConflictsMockServer 模式）。
func newMockClient(cap *syncConflictsMockCapture) (*client.FileClient, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.method = r.Method
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/resolve") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			cap.body = body
			w.Header().Set("Content-Type", "application/json")
			if cap.failSuccess {
				json.NewEncoder(w).Encode(map[string]any{"success": false})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "path": "dir/f.txt"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "conflicts": []client.SyncConflictItem{}})
	})
	ts := httptest.NewServer(mux)
	// 使用 runtime 级隔离传输客户端；server 生命周期由调用方（工厂持有）管理。
	// 为防泄漏，此处不持有 server——实际调用方应使用 newSyncConflictsMockServer。
	return client.NewFileClient(ts.URL), nil
}
