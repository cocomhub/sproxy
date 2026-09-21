// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// writeSSE 写一条 SSE 事件到 w（与服务端 writeSSEEvent 同格式：id + data + 空行 + flush）。
func writeSSE(w http.ResponseWriter, id uint64, action, owner, rel string, size int64) {
	ev := map[string]any{
		"cursor": id, "action": action, "owner": owner, "rel": rel,
	}
	if size > 0 {
		ev["size"] = size
	}
	b, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// watchMock 是 sync watch 测试的 mock 服务端：/api/events SSE + /api/sync/tasks。
// events 可编程推送（chan 串行化写 w，避免 DATA RACE）；sync 端点计数创建的 pull 任务。
type watchMock struct {
	ts      *httptest.Server
	push    func(ev map[string]any)
	disconn func()
	conns   atomic.Int32
	pulls   atomic.Int32
	pushes  atomic.Int32
	verify  atomic.Bool
	// deletePolicy 记录最后一次 push 任务的 delete_policy（默认 skip 跳过 delete 传播）。
	deletePolicy atomic.Value // string
	failAuth     atomic.Bool  // true 时 /api/events 返回 401（测轮询回退）
}

// newWatchMock 构造 watch 测试服务端。
func newWatchMock(t *testing.T) *watchMock {
	t.Helper()
	m := &watchMock{}
	pushCh := make(chan map[string]any, 16)
	disconnectCh := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if m.failAuth.Load() {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		m.conns.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, ": connected\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for {
			select {
			case ev, ok := <-pushCh:
				if !ok {
					return
				}
				id, _ := ev["id"].(uint64)
				action, _ := ev["action"].(string)
				owner, _ := ev["owner"].(string)
				rel, _ := ev["rel"].(string)
				size, _ := ev["size"].(int64)
				writeSSE(w, id, action, owner, rel, size)
			case <-disconnectCh:
				return
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req client.SyncTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Direction {
		case "pull":
			if req.VerifyAfter {
				m.verify.Store(true)
			}
			m.pulls.Add(1)
		case "push":
			m.deletePolicy.Store(req.DeletePolicy)
			m.pushes.Add(1)
		default:
			http.Error(w, `{"error":"watch 只触发 pull/push"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "watch-task", "direction": req.Direction, "status": "completed",
		})
	})
	mux.HandleFunc("GET /api/sync/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "watch-task", "direction": "pull", "status": "completed",
			"files_total": 1, "files_done": 1, "bytes_total": 10, "bytes_done": 10,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	m.ts = ts
	m.push = func(ev map[string]any) {
		select {
		case pushCh <- ev:
		default:
		}
	}
	m.disconn = func() {
		select {
		case disconnectCh <- struct{}{}:
		default:
		}
	}
	return m
}

// TestSyncCmd_Watch_Registered 验证 watch 子命令注册。
func TestSyncCmd_Watch_Registered(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdSync(factory, cli.IOStreams{}, &state.State{}, nil)
	if sub := findSubCommand(cmd, "watch"); sub == nil {
		t.Fatal("expected watch subcommand registered")
	}
}

// TestSyncCmd_Watch_RequiresRemote 验证 --remote 必填。
func TestSyncCmd_Watch_RequiresRemote(t *testing.T) {
	t.Parallel()
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected --remote required error")
	} else if !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("expected --remote error, got: %v", err)
	}
}

// TestSyncWatch_EventTriggersPull 验证事件流事件到达 → 触发一次 pull 同步任务。
// 事件动作 upload 触发；share 元事件忽略（不触发 pull）。
func TestSyncWatch_EventTriggersPull(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- cmd.ExecuteContext(ctx)
	}()

	// 等事件流连接建立。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	// 推送 upload 事件 → 应触发 pull；推送 share 事件 → 忽略。
	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})
	m.push(map[string]any{"id": uint64(2), "action": "share", "owner": "alice", "rel": "a.txt", "size": int64(10)})

	// 等 pull 任务触发（share 不触发，只有 upload 触发 1 次）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pulls.Load() >= 1
	}, func() string { return "upload 事件未触发 pull 任务" })

	// 等 300ms 窗口确认 share 未触发第二次 pull（WaitForBool 非 fatal：
	// 等 pulls>=2 出现，300ms 内没出现=未误触发=通过）。
	if testutil.WaitForBool(300*time.Millisecond, func() bool {
		return m.pulls.Load() >= 2
	}) {
		t.Fatalf("share 元事件不应触发 pull，实际触发了第 2 次")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch 退出错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_VerifyFlag 验证 --verify 透传到 pull 任务。
func TestSyncWatch_VerifyFlag(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--verify", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.verify.Load()
	}, func() string { return "--verify 未透传到 pull 任务" })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_Debounce 验证去抖窗口：窗口内连续多事件只触发一次 pull。
func TestSyncWatch_Debounce(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--debounce-ms", "2000", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	// 同一窗口内（<2s）连推 3 个 upload 事件 → 去抖只触发 1 次 pull。
	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})
	m.push(map[string]any{"id": uint64(2), "action": "upload", "owner": "alice", "rel": "b.txt", "size": int64(10)})
	m.push(map[string]any{"id": uint64(3), "action": "upload", "owner": "alice", "rel": "c.txt", "size": int64(10)})

	// 第一个事件触发 1 次 pull（窗口内后续事件合并）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pulls.Load() >= 1
	}, func() string { return "首事件未触发 pull" })

	// 短暂等待：窗口内其余事件不应再触发（WaitForBool：500ms 内 pulls>=2 未出现=去抖生效）。
	if testutil.WaitForBool(500*time.Millisecond, func() bool {
		return m.pulls.Load() >= 2
	}) {
		t.Fatalf("去抖窗口内期望 1 次 pull，实际触发了第 2 次")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_PollFallback 验证事件流 401 → 退化轮询（--poll 短间隔触发 pull）。
func TestSyncWatch_PollFallback(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	m.failAuth.Store(true) // /api/events 返回 401
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--poll", "1", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	// 401 → 退化轮询（进入即触发一次 + 每 1s 一次）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pulls.Load() >= 2
	}, func() string { return "退化轮询未触发 pull 任务" })

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_DirectionPush 验证 --direction push：upload 事件 → 触发 push 任务
// （非 pull）；默认 delete 事件跳过（不触发）；--delete-propagate 时 delete 触发。
func TestSyncWatch_DirectionPush(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--direction", "push", "--debounce-ms", "1", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	// upload 事件 → 触发 push（pushes>=1）；delete 事件默认跳过（不触发第 2 次）。
	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pushes.Load() >= 1
	}, func() string { return "upload 事件未触发 push 任务" })
	if m.pulls.Load() != 0 {
		t.Fatalf("push 方向不应触发 pull 任务，实际 pulls=%d", m.pulls.Load())
	}

	m.push(map[string]any{"id": uint64(2), "action": "delete", "owner": "alice", "rel": "a.txt", "size": int64(0)})
	if testutil.WaitForBool(300*time.Millisecond, func() bool {
		return m.pushes.Load() >= 2
	}) {
		t.Fatalf("push 方向默认应跳过 delete 事件，实际触发了第 2 次 push")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_DirectionPush_DeletePropagate 验证 --delete-propagate：delete 事件 → 触发
// push 任务且 delete_policy=propagate。
func TestSyncWatch_DirectionPush_DeletePropagate(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--direction", "push", "--delete-propagate", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	// delete 事件 → 触发 push 且 delete_policy=propagate。
	m.push(map[string]any{"id": uint64(1), "action": "delete", "owner": "alice", "rel": "a.txt", "size": int64(0)})
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pushes.Load() >= 1
	}, func() string { return "delete 事件未触发 push（--delete-propagate）" })
	if dp, _ := m.deletePolicy.Load().(string); dp != "propagate" {
		t.Fatalf("delete_policy 应为 propagate, got %q", dp)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_DirectionBoth 验证 --direction both：upload 事件 → 触发一次 push + 一次 pull
// （双向同步）；pull 结果不反触发 push（防死循环：both 只对事件本身触发一次 push+一次 pull）。
func TestSyncWatch_DirectionBoth(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--direction", "both", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})

	// 一个事件 → push>=1 且 pull>=1（双向）；且不因 pull 完成再次触发（防死循环：pushes+pulls 不超过 2）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pushes.Load() >= 1 && m.pulls.Load() >= 1
	}, func() string { return "both 方向未触发 push+pull 双向任务" })

	// 死循环防护：等待 500ms，push+pull 总数应保持 2（不多触发）。
	if testutil.WaitForBool(500*time.Millisecond, func() bool {
		return m.pushes.Load()+m.pulls.Load() > 2
	}) {
		t.Fatalf("both 模式死循环：单个事件触发了 %d+%d 次任务（期望 1+1）", m.pushes.Load(), m.pulls.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}

// TestSyncWatch_DirectionDefaultPull 验证 --direction 缺省 = pull（零回归：事件 → pull）。
func TestSyncWatch_DirectionDefaultPull(t *testing.T) {
	t.Parallel()
	m := newWatchMock(t)
	svc := client.NewFileClient(m.ts.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdSync(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"watch", "--remote", "r1", "--quiet"})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return m.conns.Load() >= 1
	}, func() string { return "事件流连接未建立" })

	m.push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(10)})
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.pulls.Load() >= 1
	}, func() string { return "默认方向事件未触发 pull 任务" })
	if m.pushes.Load() != 0 {
		t.Fatalf("默认 pull 方向不应触发 push，实际 pushes=%d", m.pushes.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch 未在 cancel 后退出")
	}
}
