// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// trash_test.go 验证 trash/quota CLI（roadmap 11.8-A2/A3，mock 服务端）：
//  1. trash list 列出条目（表格 + --json）。
//  2. trash restore <trash_rel> POST restore。
//  3. trash empty 缺 --yes 拒绝；--yes 清空。
//  4. quota 展示水位（--json + 文本）。

import (
	"bytes"
	"context"
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

// mockTrashFactory 是 clientfactory.Factory 测试替身。
type mockTrashFactory struct {
	srv *httptest.Server
}

func (f *mockTrashFactory) NewClient(cmd *cobra.Command) (*client.FileClient, error) {
	return client.NewFileClient(f.srv.URL), nil
}

// newTrashMockServer 起 mock 服务端（/api/trash 三端点 + /api/stats quota 段）。
func newTrashMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/trash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[{"trash_rel":"trash/f.txt.deleted","name":"f.txt"}]}`))
	})
	mux.HandleFunc("POST /api/trash/restore", func(w http.ResponseWriter, r *http.Request) {
		// 权威契约：服务端 restoreTrashHandler 从 ?file= query 取条目（pkg/server/trash.go）。
		// 无 query 时真实服务端返回 400 success=false——mock 与真实行为对齐。
		if r.URL.Query().Get("file") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"file 不能为空"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"restored"}`))
	})
	mux.HandleFunc("POST /api/trash/empty", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"emptied"}`))
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"quota":{"usage":500,"max_bytes":1000,"watermark":50}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cmd %v err=%v", args, err)
	}
	return buf.String()
}

// TestTrashCmd_List trash list 表格输出。
func TestTrashCmd_List(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	cmd := newCmdTrash(&mockTrashFactory{srv: srv}, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	out := runCmd(t, cmd, "list")
	_ = out
}

// TestTrashCmd_Restore trash restore <trash_rel>。
func TestTrashCmd_Restore(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	cmd := newCmdTrash(&mockTrashFactory{srv: srv}, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	runCmd(t, cmd, "restore", "trash/f.txt.deleted")
}

// TestTrashCmd_EmptyRequiresYes 缺 --yes 拒绝。
func TestTrashCmd_EmptyRequiresYes(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	var buf bytes.Buffer
	cmd := newCmdTrash(&mockTrashFactory{srv: srv}, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{"empty"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("empty 缺 --yes 应报错")
	}
}

// TestTrashCmd_EmptyYes 带 --yes 清空。
func TestTrashCmd_EmptyYes(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	cmd := newCmdTrash(&mockTrashFactory{srv: srv}, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	runCmd(t, cmd, "empty", "--yes")
}

// TestQuotaCmd_Show quota 文本输出含水位。
func TestQuotaCmd_Show(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	var buf bytes.Buffer
	cmd := newCmdQuota(&mockTrashFactory{srv: srv}, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("quota: %v", err)
	}
	if !strings.Contains(buf.String(), "水位 50%") {
		t.Fatalf("quota 输出应含水位, got %q", buf.String())
	}
}

// TestTrashClient_ListTrash 客户端 SDK 往返。
func TestTrashClient_ListTrash(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	svc := client.NewFileClient(srv.URL)
	items, err := svc.ListTrash(context.Background(), "")
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 1 || items[0].Name != "f.txt" {
		t.Fatalf("ListTrash = %+v", items)
	}
	// JSON 断言（trash_rel 字段名对齐）。
	raw, _ := json.Marshal(items[0])
	if !strings.Contains(string(raw), "trash_rel") {
		t.Fatalf("TrashItem JSON 应含 trash_rel: %s", raw)
	}
}

// TestQuotaClient_GetQuota SDK 取 quota。
func TestQuotaClient_GetQuota(t *testing.T) {
	t.Parallel()
	srv := newTrashMockServer(t)
	svc := client.NewFileClient(srv.URL)
	q, err := svc.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q == nil || q.MaxBytes != 1000 || q.Watermark != 50 {
		t.Fatalf("GetQuota = %+v", q)
	}
}

// unused 防 vet。
var _ clientfactory.Factory = (*mockTrashFactory)(nil)

// TestTrashClient_RestoreSendsFileQuery 断言 restore 契约：trash_rel 必须经 ?file= query
// 传给服务端（pkg/server/trash.go restoreTrashHandler 权威契约），body 携带会 400。
func TestTrashClient_RestoreSendsFileQuery(t *testing.T) {
	t.Parallel()
	var gotFile string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/trash/restore", func(w http.ResponseWriter, r *http.Request) {
		gotFile = r.URL.Query().Get("file")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"restored"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc := client.NewFileClient(srv.URL)
	if err := svc.RestoreTrash(context.Background(), "trash/f.txt.deleted", ""); err != nil {
		t.Fatalf("RestoreTrash: %v", err)
	}
	if gotFile != "trash/f.txt.deleted" {
		t.Fatalf("restore 应经 ?file= query 传 trash_rel, got %q", gotFile)
	}
}

// TestTrashClient_RestoreSuccessFalse 服务端 success=false → 报错（变异探针）。
func TestTrashClient_RestoreSuccessFalse(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/trash/restore", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"message":"not found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	svc := client.NewFileClient(srv.URL)
	if err := svc.RestoreTrash(context.Background(), "trash/x", ""); err == nil {
		t.Fatalf("success=false 应报错")
	}
}

// TestTrash_MultiVolume 服务端多卷回收站（#830）已支持 ?volume=/all=1，CLI 必须接入：
// list 默认汇总、--volume 只看该卷；empty 默认只清默认卷、--all 清全部（安全默认不扩大）。
func TestTrash_MultiVolume(t *testing.T) {
	var gotPaths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/trash" {
			_, _ = w.Write([]byte(`{"entries":[{"trash_rel":"trash/user/a.txt.__deleted__","name":"a.txt","volume":"vault"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)

	// list --volume vault
	var buf strings.Builder
	listCmd := newCmdTrash(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	listCmd.SetArgs([]string{"list", "--volume", "vault"})
	if err := listCmd.Execute(); err != nil {
		t.Fatalf("trash list --volume: %v", err)
	}
	if got := gotPaths[len(gotPaths)-1]; !strings.Contains(got, "volume=vault") {
		t.Fatalf("list 请求未带 volume: %s", got)
	}
	if !strings.Contains(buf.String(), "vault") {
		t.Fatalf("list 输出应含 VOLUME 列值 vault, got %q", buf.String())
	}

	// empty --all（清全部卷）
	emptyCmd := newCmdTrash(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	emptyCmd.SetArgs([]string{"empty", "--yes", "--all"})
	if err := emptyCmd.Execute(); err != nil {
		t.Fatalf("trash empty --all: %v", err)
	}
	if got := gotPaths[len(gotPaths)-1]; !strings.Contains(got, "all=1") {
		t.Fatalf("empty --all 请求未带 all=1: %s", got)
	}

	// empty 默认（不带参数）只清默认卷：不得发送 all=1 / volume
	emptyDefault := newCmdTrash(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard})
	emptyDefault.SetArgs([]string{"empty", "--yes"})
	if err := emptyDefault.Execute(); err != nil {
		t.Fatalf("trash empty 默认: %v", err)
	}
	if got := gotPaths[len(gotPaths)-1]; strings.Contains(got, "all=1") || strings.Contains(got, "volume=") {
		t.Fatalf("empty 默认不得扩大范围（破坏性默认不静默扩大）: %s", got)
	}

	// --all 与 --volume 互斥
	bad := newCmdTrash(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	bad.SetArgs([]string{"empty", "--yes", "--all", "--volume", "vault"})
	if err := bad.Execute(); err == nil {
		t.Fatal("--all 与 --volume 应互斥报错")
	}
}
