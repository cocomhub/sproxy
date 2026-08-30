// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// syncMockCapture 捕获 mock 服务端收到的创建请求体。
type syncMockCapture struct {
	req client.SyncTaskRequest
}

// newSyncMockServer 返回模拟 /api/sync/tasks 端点的测试服务器。
// finalStatus 非空时 GET 轮询第 2 次起返回该状态（测试 --wait 用）。
// POST 请求体回显到捕获器，供测试断言 flag 解析 → 请求体的映射。
func newSyncMockServer(t *testing.T, finalStatus string) (*httptest.Server, *syncMockCapture) {
	t.Helper()
	cap := &syncMockCapture{}
	var pollCount atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&cap.req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "sync-cmd-1", "direction": cap.req.Direction, "remote": cap.req.Remote,
			"src": cap.req.Src, "dst": cap.req.Dst, "conflict_policy": cap.req.ConflictPolicy,
			"recursive": cap.req.Recursive, "status": "pending",
		})
	})
	mux.HandleFunc("GET /api/sync/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := pollCount.Add(1)
		status := "syncing"
		if finalStatus != "" && n >= 2 {
			status = finalStatus
		}
		resp := map[string]any{
			"id": "sync-cmd-1", "status": status,
			"files_total": 10, "files_done": 5, "bytes_total": 1000, "bytes_done": 500,
		}
		if status == "failed" {
			resp["error"] = "connection refused"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, cap
}

// TestSyncCmd_UseAndSubcommands 验证父命令 Use 与 push/pull 子命令注册。
func TestSyncCmd_UseAndSubcommands(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{}, &state.State{}, nil)

	if cmd.Use != "sync <push|pull>" {
		t.Fatalf("expected Use 'sync <push|pull>', got %q", cmd.Use)
	}
	if sub := findSubCommand(cmd, "push"); sub == nil {
		t.Fatal("expected push subcommand")
	}
	if sub := findSubCommand(cmd, "pull"); sub == nil {
		t.Fatal("expected pull subcommand")
	}
}

// TestSyncCmd_Push_CreatesTask 验证 push flag 解析 → CreateSyncTask 请求体 + 简洁输出。
func TestSyncCmd_Push_CreatesTask(t *testing.T) {
	mock, cap := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "a/b.txt", "--dst", "x/y.txt",
		"--recursive", "--conflict", "lww", "--sync-empty-dirs", "--follow-symlinks"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync push failed: %v", err)
	}

	if cap.req.Direction != "push" {
		t.Fatalf("want direction push, got %q", cap.req.Direction)
	}
	if cap.req.Remote != "r1" || cap.req.Src != "a/b.txt" || cap.req.Dst != "x/y.txt" {
		t.Fatalf("request mismatch: %+v", cap.req)
	}
	if !cap.req.Recursive || !cap.req.SyncEmptyDirs || !cap.req.FollowSymlinks {
		t.Fatalf("bool flags not mapped to request: %+v", cap.req)
	}
	if cap.req.ConflictPolicy != "lww" {
		t.Fatalf("want conflict_policy lww, got %q", cap.req.ConflictPolicy)
	}
	if !strings.Contains(buf.String(), "sync-cmd-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

// TestSyncCmd_Push_IncludeExclude 验证 include/exclude 传递。
func TestSyncCmd_Push_IncludeExclude(t *testing.T) {
	mock, cap := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--include", "*.go", "--include", "*.md", "--exclude", "*.tmp"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync push failed: %v", err)
	}
	if len(cap.req.Include) != 2 || cap.req.Include[0] != "*.go" || cap.req.Include[1] != "*.md" {
		t.Fatalf("include mismatch: %v", cap.req.Include)
	}
	if len(cap.req.Exclude) != 1 || cap.req.Exclude[0] != "*.tmp" {
		t.Fatalf("exclude mismatch: %v", cap.req.Exclude)
	}
}

// TestSyncCmd_Push_RequiresRemote 验证 --remote 必填。
func TestSyncCmd_Push_RequiresRemote(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--src", "x.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --remote missing")
	}
	if !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("expected error to mention --remote, got: %v", err)
	}
}

// TestSyncCmd_Push_InvalidConflict 验证非法 --conflict 值报错。
func TestSyncCmd_Push_InvalidConflict(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--conflict", "bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid conflict policy")
	}
	if !strings.Contains(err.Error(), "--conflict") {
		t.Fatalf("expected error to mention --conflict, got: %v", err)
	}
}

// TestSyncCmd_Push_ConflictRenameMapsToUnderscore 验证 CLI conflict-rename → 服务端 conflict_rename。
func TestSyncCmd_Push_ConflictRenameMapsToUnderscore(t *testing.T) {
	mock, cap := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--conflict", "conflict-rename"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync push failed: %v", err)
	}
	if cap.req.ConflictPolicy != "conflict_rename" {
		t.Fatalf("want conflict_policy conflict_rename, got %q", cap.req.ConflictPolicy)
	}
}

// TestSyncCmd_Pull_Direction 验证 pull 请求体方向与 src/dst 语义。
func TestSyncCmd_Pull_Direction(t *testing.T) {
	mock, cap := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"pull", "--remote", "r1", "--src", "remote/dir", "--dst", "local/dir"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync pull failed: %v", err)
	}
	if cap.req.Direction != "pull" {
		t.Fatalf("want direction pull, got %q", cap.req.Direction)
	}
	if cap.req.Src != "remote/dir" || cap.req.Dst != "local/dir" {
		t.Fatalf("request mismatch: %+v", cap.req)
	}
}

// TestSyncCmd_Push_Wait 验证 --wait 轮询直至 completed 并展示进度。
func TestSyncCmd_Push_Wait(t *testing.T) {
	mock, _ := newSyncMockServer(t, "completed")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "x.txt", "--wait",
		"--poll-interval", "50ms", "--timeout", "10s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sync push --wait failed: %v", err)
	}
	if !strings.Contains(buf.String(), "sync-cmd-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "completed") {
		t.Fatalf("expected completed status in output, got: %s", buf.String())
	}
}

// TestSyncCmd_Push_Wait_Failed 验证 --wait 任务失败返回非零并打印 Error。
func TestSyncCmd_Push_Wait_Failed(t *testing.T) {
	mock, _ := newSyncMockServer(t, "failed")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "x.txt", "--wait",
		"--poll-interval", "50ms", "--timeout", "10s"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when task fails")
	}
	if !strings.Contains(err.Error(), "sync-cmd-1") {
		t.Fatalf("expected error to mention task ID, got: %v", err)
	}
	if !strings.Contains(buf.String(), "failed") {
		t.Fatalf("expected failure status in output, got: %s", buf.String())
	}
}

// TestSyncCmd_Push_Wait_JSON 验证 --wait --json 组合：stdout 为纯 JSON（终态任务），
// 不夹带"等待..."等人工提示行，脚本可整段解析。
func TestSyncCmd_Push_Wait_JSON(t *testing.T) {
	mock, _ := newSyncMockServer(t, "completed")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	root.SetArgs([]string{"sync", "push", "--remote", "r1", "--src", "x.txt",
		"--wait", "--poll-interval", "50ms", "--timeout", "10s", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sync push --wait --json failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("expected pure JSON output, got: %q (err=%v)", buf.String(), err)
	}
	if out["id"] != "sync-cmd-1" {
		t.Fatalf("expected id sync-cmd-1 in JSON, got: %v", out["id"])
	}
	if out["status"] != "completed" {
		t.Fatalf("expected status completed in JSON, got: %v", out["status"])
	}
}

// TestSyncCmd_Push_JSON 验证 --json 输出全量任务 JSON。
// --json 是根命令持久化 flag：经根命令执行（root.SetArgs），验证
// "sclient sync push --json" 与 "sclient --json sync push" 行为一致。
func TestSyncCmd_Push_JSON(t *testing.T) {
	mock, _ := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	root.SetArgs([]string{"sync", "push", "--remote", "r1", "--src", "x.txt", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sync push --json failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("expected JSON output, got: %q (err=%v)", buf.String(), err)
	}
	if out["id"] != "sync-cmd-1" {
		t.Fatalf("expected id sync-cmd-1 in JSON, got: %v", out["id"])
	}
	if out["direction"] != "push" {
		t.Fatalf("expected direction push in JSON, got: %v", out["direction"])
	}
}

// TestSyncCmd_Push_JSON_BeforeSubcommand 验证根级 --json（sclient --json sync push）同样生效。
func TestSyncCmd_Push_JSON_BeforeSubcommand(t *testing.T) {
	mock, _ := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	root.SetArgs([]string{"--json", "sync", "push", "--remote", "r1", "--src", "x.txt"})
	if err := root.Execute(); err != nil {
		t.Fatalf("sync push --json (root-level) failed: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		t.Fatalf("expected JSON output, got: %q (err=%v)", buf.String(), err)
	}
	if out["id"] != "sync-cmd-1" {
		t.Fatalf("expected id sync-cmd-1 in JSON, got: %v", out["id"])
	}
}

// TestSyncCmd_Push_CreatedTaskFailed 验证创建响应即 failed 时命令非零退出。
func TestSyncCmd_Push_CreatedTaskFailed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "sync-fail", "direction": "push", "remote": "r1",
			"status": "failed", "error": "remote unreachable",
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "x.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when created task is already failed")
	}
	if !strings.Contains(err.Error(), "sync-fail") {
		t.Fatalf("expected error to mention task ID, got: %v", err)
	}
	if !strings.Contains(buf.String(), "sync-fail") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

// TestSyncCmd_Push_Wait_JSON_Failed 验证 --wait --json + failed：stdout 为纯 JSON
// （status=failed），且退出码非零（审查 I-1 回归：--json 提前 return 曾跳过状态检查）。
func TestSyncCmd_Push_Wait_JSON_Failed(t *testing.T) {
	mock, _ := newSyncMockServer(t, "failed")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	root.SetArgs([]string{"sync", "push", "--remote", "r1", "--src", "x.txt",
		"--wait", "--poll-interval", "50ms", "--timeout", "10s", "--json"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit when task fails with --json")
	}
	var out map[string]any
	if uerr := json.Unmarshal([]byte(buf.String()), &out); uerr != nil {
		t.Fatalf("expected pure JSON output, got: %q (err=%v)", buf.String(), uerr)
	}
	if out["status"] != "failed" {
		t.Fatalf("expected status failed in JSON, got: %v", out["status"])
	}
}

// TestSyncCmd_Push_JSON_CreatedTaskFailed 验证创建即 failed + --json：输出 JSON 且非零退出
// （审查 I-1 回归）。
func TestSyncCmd_Push_JSON_CreatedTaskFailed(t *testing.T) {
	mock, _ := newSyncMockServer(t, "")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root := &cobra.Command{}
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(cmd)
	// 用 createFailedMockServer（POST 返回 failed 任务）
	fm := newCreateFailedMockServer(t)
	defer fm.Close()
	factory2 := clientfactory.NewMock(client.NewFileClient(fm.URL), nil)
	cmd2 := NewCmdSync(factory2, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	root2 := &cobra.Command{}
	root2.PersistentFlags().Bool("json", false, "")
	root2.AddCommand(cmd2)
	root2.SetArgs([]string{"sync", "push", "--remote", "r1", "--src", "x.txt", "--json"})
	err := root2.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit when created task is failed with --json")
	}
	var out map[string]any
	if uerr := json.Unmarshal([]byte(buf.String()), &out); uerr != nil {
		t.Fatalf("expected JSON output, got: %q (err=%v)", buf.String(), uerr)
	}
	if out["status"] != "failed" {
		t.Fatalf("expected status failed in JSON, got: %v", out["status"])
	}
}

// newCreateFailedMockServer 返回 POST /api/sync/tasks 即返回 failed 任务的 mock。
func newCreateFailedMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "sync-fail", "status": "failed", "error": "boom",
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestSyncCmd_Push_Wait_Cancelled 验证 --wait + cancelled → 非零退出（对齐云端 cancelled 计入失败）。
func TestSyncCmd_Push_Wait_Cancelled(t *testing.T) {
	mock, _ := newSyncMockServer(t, "cancelled")
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "x.txt", "--wait",
		"--poll-interval", "50ms", "--timeout", "10s"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected non-zero exit when task cancelled")
	}
	if !strings.Contains(err.Error(), "取消") {
		t.Fatalf("expected error to mention cancel, got: %v", err)
	}
}

// TestSyncCmd_Wait_TimeoutExpired 验证 --wait --timeout 过期返回 error（不伪造终态）。
func TestSyncCmd_Wait_TimeoutExpired(t *testing.T) {
	mock, _ := newSyncMockServer(t, "") // 恒 syncing
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"push", "--remote", "r1", "--src", "x.txt", "--wait",
		"--poll-interval", "50ms", "--timeout", "100ms"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when wait timeout expires")
	}
}
