// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventBus_PublishAndSubscribe 验证事件总线：publish 广播给 owner 订阅者 + 游标单调。
func TestEventBus_PublishAndSubscribe(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	ch, _, cancel := bus.Subscribe("alice")
	defer cancel()

	bus.OnFileEvent("upload", "alice", "a.txt", 123)
	select {
	case ev := <-ch:
		if ev.Action != "upload" || ev.Owner != "alice" || ev.Rel != "a.txt" || ev.Size != 123 {
			t.Fatalf("事件内容不符: %+v", ev)
		}
		if ev.Cursor != 1 {
			t.Fatalf("游标应=1, got %d", ev.Cursor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者未收到事件")
	}
	// 不同 owner 隔离：bob 事件不影响 alice 订阅（无新事件）。
	bus.OnFileEvent("delete", "bob", "b.txt", 0)
	select {
	case ev := <-ch:
		t.Fatalf("bob 事件不应广播给 alice: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// 期望无事件
	}
}

// TestEventBus_Replay 验证重连回放：Last-Event-ID 后的事件可回放，游标单调。
func TestEventBus_Replay(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	bus.OnFileEvent("upload", "alice", "a.txt", 1)
	bus.OnFileEvent("upload", "alice", "b.txt", 2)
	bus.OnFileEvent("delete", "alice", "a.txt", 0)

	evs, cur, ok := bus.Replay("alice", 1) // 从游标 1 之后（不含 1）
	if !ok {
		t.Fatal("应可回放")
	}
	if cur != 3 {
		t.Fatalf("最新游标应=3, got %d", cur)
	}
	if len(evs) != 2 {
		t.Fatalf("应回放 2 条（b.txt upload + a.txt delete）, got %d", len(evs))
	}
	if evs[0].Action != "upload" || evs[0].Rel != "b.txt" {
		t.Fatalf("首条应 b.txt upload, got %+v", evs[0])
	}
	// 无新事件：空回放。
	evs2, cur2, ok2 := bus.Replay("alice", 3)
	if !ok2 || len(evs2) != 0 || cur2 != 3 {
		t.Fatalf("无新事件应空回放, got %d/%d", len(evs2), cur2)
	}
	// 落后过多滚出缓冲：游标 0 但事件超 cap。
	bus2 := NewEventBus()
	for i := range eventRingCap + 10 {
		bus2.OnFileEvent("upload", "bob", "f", int64(i))
	}
	if _, _, ok3 := bus2.Replay("bob", 0); ok3 {
		t.Fatal("落后过多应回放失败")
	}
}

// TestEventsHandler_SSE 验证 GET /api/events SSE：上传触发事件推送到订阅流。
func TestEventsHandler_SSE(t *testing.T) {
	// 使用 httptest.Server（真实 HTTP + 流式响应）——串行：SSE 流需持续连接。
	// sproxy:serial: SSE 长连接 + 上传并发的时序依赖（httptest 流式读需独占连接）。
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 订阅 SSE（带 auth：testServer 默认无 auth）。
	req, _ := http.NewRequest(http.MethodGet, url+"/api/events?owner=anonymous", nil)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应 text/event-stream, got %q", ct)
	}
	// 首帧注释。
	br := bufio.NewReader(resp.Body)
	line, _ := br.ReadString('\n')
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("首帧应为注释心跳, got %q", line)
	}
	// 忽略到空行。
	for {
		l, _ := br.ReadString('\n')
		if l == "\n" {
			break
		}
	}

	// 触发上传（另一 goroutine，SSE 流阻塞读）。
	done := make(chan error, 1)
	go func() {
		status, body := uploadFile(t, url, "event.txt", []byte("event-data"), map[string]string{
			headerFileChecksum: sha256hex([]byte("event-data")),
		})
		if status != http.StatusOK {
			done <- &httpError{status: status, body: string(body)}
			return
		}
		done <- nil
	}()
	// 读 SSE 事件。
	var gotID, gotData string
	deadline := time.After(5 * time.Second)
	for gotData == "" {
		select {
		case <-deadline:
			t.Fatal("SSE 未收到事件")
		default:
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 SSE 行: %v", err)
		}
		if after, ok := strings.CutPrefix(line, "id: "); ok {
			gotID = strings.TrimSpace(after)
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			gotData = strings.TrimSpace(after)
		}
	}
	if gotID == "" {
		t.Fatal("未收到事件 id")
	}
	var ev fileEvent
	if err := json.Unmarshal([]byte(gotData), &ev); err != nil {
		t.Fatalf("事件 data 解析失败: %v (%q)", err, gotData)
	}
	if ev.Action != "upload" || ev.Rel != "event.txt" {
		t.Fatalf("事件内容不符: %+v", ev)
	}
	if err := <-done; err != nil {
		t.Fatalf("上传: %v", err)
	}
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return "status=" + string(rune(e.status)) + " body=" + e.body }
