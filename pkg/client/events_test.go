// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// writeSSE 写一条 SSE 事件到 w（与服务端 writeSSEEvent 同格式：id + data + 空行）。
func writeSSE(w http.ResponseWriter, id uint64, action, owner, rel string, size int64) {
	ev := map[string]any{
		"cursor": id, "action": action, "owner": owner, "rel": rel,
	}
	if size > 0 {
		ev["size"] = size
	}
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// newEventsMockServer 返回一个可编程 SSE 服务端：每次连接写入 events（首条注释心跳），
// 断开后（client 端 ctx 取消或服务器关闭连接）支持重连计数。
// 返回的 push 函数向当前连接的客户端写事件；disconnect 断开当前连接（模拟断线）。
func newEventsMockServer(t *testing.T) (*httptest.Server, func(ev map[string]any), func(), *atomic32) {
	t.Helper()
	var mu sync.Mutex
	var conns int
	connCount := &atomic32{}
	// pushCh 串行化事件写入：push 只投递（任意 goroutine），handler 内消费并写 w
	// （所有写 w 都在 handler goroutine，天然无 DATA RACE）。
	pushCh := make(chan map[string]any, 8)
	disconnectCh := make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		mu.Unlock()
		connCount.Add(1)

		hdr := w.Header()
		hdr.Set("Content-Type", "text/event-stream")
		hdr.Set("Cache-Control", "no-cache")
		fmt.Fprint(w, ": connected\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// 消费 push 投递的事件；客户端断开（ctx done）或 pushCh 关闭即退出。
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
			case <-r.Context().Done():
				return
			case <-disconnectCh:
				return
			}
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// push 只投递（handler 内写 w，跨 goroutine 无竞态）。
	push := func(ev map[string]any) {
		select {
		case pushCh <- ev:
		default:
			// 队列满（无 handler 消费 = 无连接）：丢弃（测试逻辑错误，安全忽略）。
		}
	}
	disconnect := func() {
		// 断开当前连接（从客户端视角断线）：通知 handler 退出（handler 内正常
		// return，w 由 http 层统一关闭——避免 Hijack 与 handler 写 w 的竞态）。
		select {
		case disconnectCh <- struct{}{}:
		default:
		}
	}
	return ts, push, disconnect, connCount
}

// atomic32 是测试用的简单计数器（避免 import sync/atomic 的繁琐）。
type atomic32 struct {
	mu sync.Mutex
	n  int
}

func (a *atomic32) Add(d int) { a.mu.Lock(); a.n += d; a.mu.Unlock() }
func (a *atomic32) Load() int { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

// TestWatchEvents_ReceivesUploadEvent 验证 WatchEvents 收到 upload 事件（含游标/字段）。
func TestWatchEvents_ReceivesUploadEvent(t *testing.T) {
	ts, push, _, _ := newEventsMockServer(t)

	svc := NewFileClient(ts.URL)
	got := make(chan FileEvent, 1)
	ctx := t.Context()

	go func() {
		_ = svc.WatchEvents(ctx, WatchEventsOptions{Owner: "alice"}, func(ev FileEvent) {
			select {
			case got <- ev:
			default:
			}
		})
	}()

	// 等服务端连接建立 + 推送事件，收到即验证（带总超时防 hang）。
	done := make(chan FileEvent, 1)
	// 循环推送直到收到事件（连接建立前的推送无害——无连接时 push 为空操作）。
	go func() {
		for {
			push(map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a/b.txt", "size": int64(42)})
			<-time.After(100 * time.Millisecond)
		}
	}()
	var ev FileEvent
	select {
	case ev = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("WatchEvents 未收到 upload 事件（10s 超时）")
	}
	_ = done
	if ev.Action != "upload" {
		t.Fatalf("want upload, got %+v", ev)
	}
	if ev.Rel != "a/b.txt" {
		t.Fatalf("want a/b.txt, got %+v", ev)
	}
	if ev.Owner != "alice" {
		t.Fatalf("want alice, got %+v", ev)
	}
	if ev.Cursor != 1 {
		t.Fatalf("want cursor 1, got %d", ev.Cursor)
	}
}

// TestWatchEvents_CursorReplayOnReconnect 验证断线重连携带 Last-Event-ID（游标回放）。
// 服务端记录每次连接的 Last-Event-ID 头；第一次连接无游标（0），第二次连接带上次游标。
func TestWatchEvents_CursorReplayOnReconnect(t *testing.T) {
	var mu sync.Mutex
	var lastIDs []string
	var conns int
	var curConnPush chan map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": connected\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		disconnectCh := make(chan struct{}, 1)
		pushCh := make(chan map[string]any, 8)
		mu.Lock()
		curConnPush = pushCh
		mu.Unlock()
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
			case <-r.Context().Done():
				return
			case <-disconnectCh:
				return
			}
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	var gotEvents []FileEvent
	var muGot sync.Mutex
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = svc.WatchEvents(ctx, WatchEventsOptions{Owner: "alice"}, func(ev FileEvent) {
			muGot.Lock()
			gotEvents = append(gotEvents, ev)
			muGot.Unlock()
		})
	}()

	// 等第一次连接建立 + 推送事件 + 断开 → 触发重连。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		n := conns
		curP := curConnPush
		mu.Unlock()
		return n >= 1 && curP != nil
	}, func() string { return "第一次连接未建立" })
	mu.Lock()
	curP := curConnPush
	mu.Unlock()
	curP <- map[string]any{"id": uint64(1), "action": "upload", "owner": "alice", "rel": "a.txt", "size": int64(1)}
	// 等客户端消费事件（gotEvents 有 1 条）再断线——确保游标已推进到 1 才重连。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		muGot.Lock()
		defer muGot.Unlock()
		return len(gotEvents) >= 1
	}, func() string { return "客户端未消费 upload 事件" })
	close(curP)

	// 等第二次连接（重连）携带 Last-Event-ID=1。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return conns >= 2
	}, func() string { return "重连未发生" })
	// 先断开客户端（第 2 次连接 handler 才能退出），再断言——否则 ts.Close 等挂起连接超时。
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) < 2 || lastIDs[1] != "1" {
		t.Fatalf("重连 Last-Event-ID 应为 1, got %v", lastIDs)
	}
}

// 辅助：从 server 指针取当前连接 w 并写事件。

// 辅助：关闭当前连接（模拟断线）。

// TestWatchEvents_UnauthorizedFatal 验证 401 认证失败直接返回 error（不重连）。
func TestWatchEvents_UnauthorizedFatal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	err := svc.WatchEvents(context.Background(), WatchEventsOptions{Owner: "alice"}, nil)
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected error to mention 401, got: %v", err)
	}
}

// TestWatchEvents_CtxCancel 验证 ctx 取消后 WatchEvents 正常返回 nil。
func TestWatchEvents_CtxCancel(t *testing.T) {
	ts, _, _, _ := newEventsMockServer(t)

	svc := NewFileClient(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := svc.WatchEvents(ctx, WatchEventsOptions{Owner: "alice"}, nil)
	if err != nil {
		t.Fatalf("ctx cancel 应返回 nil, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("ctx cancel 后未及时返回")
	}
}

var _ = io.Discard // 保留 io 引用（部分 helper 未来使用）
