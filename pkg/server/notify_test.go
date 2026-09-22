// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// notify_test.go 验证通知中心框架（roadmap P0 通知中心）：
//  1. 规则路由：action glob（* / upload / delete）→ channels 命中。
//  2. 去抖：同 action+object 在窗口内只发一次；窗口外恢复（再触发再发）。
//  3. 历史：记录成功/失败 + 去抖跳过的条目（有界 200）。
//  4. 重试：渠道失败 → 指数退避重试 3 次后失败进历史。
//  5. wecom/serverchan 渠道：真实 HTTP POST（httptest mock 校验载荷）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// fakeNotifier 记录调用（测试用渠道）。
type fakeNotifier struct {
	name    string
	mu      sync.Mutex
	calls   []NotifyMessage
	fail    atomic.Bool
	latency atomic.Int64
}

func (f *fakeNotifier) Name() string { return f.name }

func (f *fakeNotifier) Send(ctx context.Context, m NotifyMessage) error {
	if f.fail.Load() {
		return &NotifierError{Channel: f.name, Err: io.EOF}
	}
	if d := f.latency.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, m)
	return nil
}

func (f *fakeNotifier) callsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestNotifyCenter_RuleRouting 规则路由 + 渠道分发。
func TestNotifyCenter_RuleRouting(t *testing.T) {
	t.Parallel()
	wecom := &fakeNotifier{name: "wecom"}
	sc := &fakeNotifier{name: "serverchan"}
	nc := NewNotifyCenter(NotifyConfig{
		Enabled: true,
		Rules: []NotifyRule{
			{Action: "upload", Channels: []string{"wecom"}},
			{Action: "delete", Channels: []string{"wecom", "serverchan"}},
		},
	}, nil)
	nc.Register(wecom)
	nc.Register(sc)
	t.Cleanup(nc.Close)

	nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	nc.Dispatch(context.Background(), AuditEvent{Action: "delete", Object: "b.txt", Result: "success"})
	nc.Dispatch(context.Background(), AuditEvent{Action: "rename", Object: "c.txt", Result: "success"})

	waitNotify(t, func() bool { return wecom.callsCount() >= 2 && sc.callsCount() >= 1 })
	if wecom.callsCount() != 2 {
		t.Fatalf("wecom 应收到 2 条（upload+delete），got %d", wecom.callsCount())
	}
	if sc.callsCount() != 1 {
		t.Fatalf("serverchan 应收到 1 条（delete），got %d", sc.callsCount())
	}
}

// TestNotifyCenter_Debounce 同 action+object 窗口内去抖。
func TestNotifyCenter_Debounce(t *testing.T) {
	t.Parallel()
	ch := &fakeNotifier{name: "wecom"}
	nc := NewNotifyCenter(NotifyConfig{
		Enabled:  true,
		Rules:    []NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
		Debounce: 50 * time.Millisecond,
	}, nil)
	nc.Register(ch)
	t.Cleanup(nc.Close)

	// 窗口内连续 3 次 → 只发 1 条。
	for range 3 {
		nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	}
	time.Sleep(80 * time.Millisecond)
	if got := ch.callsCount(); got != 1 {
		t.Fatalf("去抖窗口内应只发 1 条，got %d", got)
	}
	// 窗口外（Debounce 后）再触发 → 再发。
	time.Sleep(60 * time.Millisecond)
	nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	waitNotify(t, func() bool { return ch.callsCount() == 2 })
}

// TestNotifyCenter_History 历史记录（成功/去抖跳过/失败）。
func TestNotifyCenter_History(t *testing.T) {
	t.Parallel()
	ch := &fakeNotifier{name: "wecom"}
	nc := NewNotifyCenter(NotifyConfig{
		Enabled:  true,
		Rules:    []NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
		Debounce: time.Hour, // 长窗口：第 2 次必去抖
	}, nil)
	nc.Register(ch)
	t.Cleanup(nc.Close)

	nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	waitNotify(t, func() bool { return len(nc.History()) >= 2 })
	hist := nc.History()
	// 两条 dispatch goroutine 并发：addHistory 顺序不定——断言两种状态各存在一条。
	var sent, debounced bool
	for _, e := range hist {
		if e.Status == "sent" {
			sent = true
		}
		if e.Status == "debounced" {
			debounced = true
		}
	}
	if !sent || !debounced {
		t.Fatalf("历史应含 sent 与 debounced 各一：got %v", histStatuses(hist))
	}
}

// TestNotifyCenter_Retry 渠道失败 → 重试 3 次 → 历史 failed。
func TestNotifyCenter_Retry(t *testing.T) {
	t.Parallel()
	ch := &fakeNotifier{name: "wecom"}
	ch.fail.Store(true)
	nc := NewNotifyCenter(NotifyConfig{
		Enabled:   true,
		Rules:     []NotifyRule{{Action: "*", Channels: []string{"wecom"}}},
		Retry:     3,
		RetryBase: 10 * time.Millisecond, // 快退避（测试）
	}, nil)
	nc.Register(ch)
	t.Cleanup(nc.Close)

	nc.Dispatch(context.Background(), AuditEvent{Action: "upload", Object: "a.txt", Result: "success"})
	waitNotify(t, func() bool { return len(nc.History()) >= 1 })
	if got := len(nc.History()); got < 1 {
		t.Fatalf("无历史")
	}
	if nc.History()[0].Status != "failed" {
		t.Fatalf("失败应 failed，got %s", nc.History()[0].Status)
	}
	if attempts := nc.attempts("wecom"); attempts < 3 {
		t.Fatalf("重试次数 = %d, want >= 3", attempts)
	}
}

// TestNotifyCenter_WecomChannel 企微机器人渠道：真实 POST + 载荷校验。
func TestNotifyCenter_WecomChannel(t *testing.T) {
	t.Parallel()
	var got map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	ch := NewWecomNotifier(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "通知", Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	md, _ := got["markdown"].(map[string]any)
	if md == nil {
		t.Fatalf("载荷无 markdown: %v", got)
	}
	if !strings.Contains(md["content"].(string), "hello") {
		t.Fatalf("content 应含 hello: %v", md["content"])
	}
}

// TestNotifyCenter_ServerChanChannel Server 酱渠道：真实 POST + 载荷校验。
func TestNotifyCenter_ServerChanChannel(t *testing.T) {
	t.Parallel()
	var path string
	var body []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		path = r.URL.Path
		body = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer srv.Close()

	ch := NewServerChanNotifier(srv.URL+"/{key}.send", "SCT-test")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "通知", Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasSuffix(path, "/SCT-test.send") {
		t.Fatalf("path = %q, want /{key}.send", path)
	}
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body 应含 hello: %q", body)
	}
}

// histStatuses 返回历史状态列表（调试）。
func histStatuses(h []historyEntry) []string {
	out := make([]string, len(h))
	for i, e := range h {
		out[i] = e.Status
	}
	return out
}

// waitNotify 轮询条件（短超时；通知分发是异步 goroutine）。
func waitNotify(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitNotify 超时")
}

// TestNotifyEndpoints /api/notify/history 与 /api/notify/test（经 h 装配）。
func TestNotifyEndpoints(t *testing.T) {
	t.Parallel()
	// 装配带通知中心的服务（wecom 渠道 → mock webhook）。
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()
	url, _, _ := newTestServer(t, func(c *Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []NotifyRule{{Action: "*", Channels: []string{"wecom"}}}
		c.Notify.Channels.Wecom.Webhook = srv.URL
	})
	// 无凭据（allowInsecureLoopback）→ localMux 隧道内层或 authMiddleware……
	// newTestServer 是 noAuth（allowInsecureLoopback）→ /api/notify/history 应可读。
	resp, err := http.Get(url + "/api/notify/history")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "entries") {
		t.Fatalf("history: status=%d body=%q", resp.StatusCode, body[:min(len(body), 60)])
	}
	// POST test（自检 wecom 渠道）。
	req, _ := http.NewRequest(http.MethodPost, url+"/api/notify/test", nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	resp2, err := cl.Do(req)
	if err != nil {
		t.Fatalf("POST test: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(body2), `"wecom":"ok"`) {
		t.Fatalf("test: status=%d body=%q", resp2.StatusCode, body2)
	}
}
