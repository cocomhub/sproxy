// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestEventBus_Publish 验证公开 Publish 方法：与 OnFileEvent 相同语义（入环 + 广播 + 游标单调）。
// 装配层 handler（version/share）用它发布 files.EventSink 之外的事件源。
func TestEventBus_Publish(t *testing.T) {
	t.Parallel()
	bus := NewEventBus()
	ch, _, cancel := bus.Subscribe("alice")
	defer cancel()

	bus.Publish("version", "alice", "v.txt", 42)
	select {
	case ev := <-ch:
		if ev.Action != "version" || ev.Owner != "alice" || ev.Rel != "v.txt" || ev.Size != 42 {
			t.Fatalf("Publish 事件内容不符: %+v", ev)
		}
		if ev.Cursor != 1 {
			t.Fatalf("游标应=1, got %d", ev.Cursor)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 后订阅者未收到事件")
	}
	// 与 OnFileEvent 共享同一环（游标续增）：Publish 后 Replay 可回放。
	bus.OnFileEvent("upload", "alice", "a.txt", 1)
	evs, cur, ok := bus.Replay("alice", 1)
	if !ok || cur != 2 || len(evs) != 1 || evs[0].Action != "upload" {
		t.Fatalf("Publish 后 Replay 异常: ok=%v cur=%d evs=%+v", ok, cur, evs)
	}
}

// TestVersionRestore_PublishesEvent 验证版本恢复发布 version 事件（订阅者可感知内容回滚）。
func TestVersionRestore_PublishesEvent(t *testing.T) {
	// sproxy:serial: httptest 全链路 + 上传两版 + restore 的时序依赖（与 TestEventsHandler_SSE 同模式）。
	url, h := newEventTestServer(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// 上传两版，产生版本历史。
	if st, _ := uploadFile(t, url, "ver.txt", []byte("version 1"), map[string]string{
		headerFileChecksum: sha256hex([]byte("version 1")),
	}); st != http.StatusOK {
		t.Fatalf("upload v1 status = %d", st)
	}
	if st, _ := uploadFile(t, url, "ver.txt", []byte("version 2"), map[string]string{
		headerFileChecksum: sha256hex([]byte("version 2")),
	}); st != http.StatusOK {
		t.Fatalf("upload v2 status = %d", st)
	}

	// 列出版本拿 version_id。
	listReq, _ := http.NewRequest(http.MethodGet, url+"/api/versions?filename=ver.txt", nil)
	listResp, err := testHTTPClient(t).Do(listReq)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listResult)
	listResp.Body.Close()
	if len(listResult.Versions) == 0 {
		t.Fatal("expected versions")
	}
	versionID := listResult.Versions[0].VersionID

	// 订阅事件总线（testServer no-auth → owner=anonymous）。
	ch, _, cancel := h.eventBus().Subscribe("anonymous")
	defer cancel()

	// 触发恢复。
	restoreURL := fmt.Sprintf("%s/api/versions/restore?filename=ver.txt&version_id=%d", url, versionID)
	restoreReq, _ := http.NewRequest(http.MethodPost, restoreURL, nil)
	restoreResp, err := testHTTPClient(t).Do(restoreReq)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", restoreResp.StatusCode)
	}

	select {
	case ev := <-ch:
		if ev.Action != "version" || ev.Rel != "ver.txt" {
			t.Fatalf("restore 事件内容不符: %+v", ev)
		}
		if ev.Size <= 0 {
			t.Fatalf("restore 事件 size 应>0（恢复后文件大小）, got %d", ev.Size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore 后未收到 version 事件")
	}
}

// TestShareCreate_PublishesEvent 验证分享创建发布 share 事件（订阅者可感知授权变更；载荷不含 token）。
func TestShareCreate_PublishesEvent(t *testing.T) {
	// sproxy:serial: httptest 全链路 + 上传 + 分享创建的时序依赖（与 TestEventsHandler_SSE 同模式）。
	url, h := newEventTestServer(t, nil)

	// 上传文件。
	body := []byte("shared content")
	if st, _ := uploadFile(t, url, "shared.txt", body, map[string]string{
		headerFileChecksum: sha256hex(body),
	}); st != http.StatusOK {
		t.Fatalf("upload status = %d", st)
	}

	ch, _, cancel := h.eventBus().Subscribe("anonymous")
	defer cancel()

	// 创建分享。
	reqBody := `{"filename":"shared.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create share status = %d, want 200", resp.StatusCode)
	}

	select {
	case ev := <-ch:
		if ev.Action != "share" || ev.Rel != "shared.txt" {
			t.Fatalf("share 事件内容不符: %+v", ev)
		}
		if ev.Size <= 0 {
			t.Fatalf("share 事件 size 应>0（分享文件大小）, got %d", ev.Size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("share 创建后未收到 share 事件")
	}
}

// newEventTestServer 建 (URL, *Handlers) 同实例的测试服务器（no-auth 装配）：
// 事件订阅须与 HTTP 请求打到同一 Handlers（EventBus 单例在其上），
// 故不能用 newTestServerWithAllRoutes（不暴露 h）——注册后返回 h 供测试订阅。
func newEventTestServer(t *testing.T, modifyCfg func(*Config)) (string, *Handlers) {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts.URL, h
}

// TestVersionDelete_PublishesEvent 验证版本删除发布 version 事件（size=0：删除非内容变更）。
func TestVersionDelete_PublishesEvent(t *testing.T) {
	// sproxy:serial: httptest 全链路 + 上传两版 + 删除版本的时序依赖（与 TestEventsHandler_SSE 同模式）。
	url, h := newEventTestServer(t, func(cfg *Config) {
		cfg.Versioning.Enabled = true
		cfg.Versioning.MaxVersions = 10
	})

	// 上传两版，产生版本历史。
	if st, _ := uploadFile(t, url, "dv.txt", []byte("v1"), map[string]string{
		headerFileChecksum: sha256hex([]byte("v1")),
	}); st != http.StatusOK {
		t.Fatalf("upload v1 status = %d", st)
	}
	if st, _ := uploadFile(t, url, "dv.txt", []byte("v2"), map[string]string{
		headerFileChecksum: sha256hex([]byte("v2")),
	}); st != http.StatusOK {
		t.Fatalf("upload v2 status = %d", st)
	}

	listReq, _ := http.NewRequest(http.MethodGet, url+"/api/versions?filename=dv.txt", nil)
	listResp, err := testHTTPClient(t).Do(listReq)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	var listResult struct {
		Versions []VersionInfo `json:"versions"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&listResult)
	listResp.Body.Close()
	if len(listResult.Versions) == 0 {
		t.Fatal("expected versions")
	}
	versionID := listResult.Versions[0].VersionID

	ch, _, cancel := h.eventBus().Subscribe("anonymous")
	defer cancel()

	delReq, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/api/versions?filename=dv.txt&version_id=%d", url, versionID), nil)
	delResp, err := testHTTPClient(t).Do(delReq)
	if err != nil {
		t.Fatalf("delete version: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete version status = %d, want 200", delResp.StatusCode)
	}

	select {
	case ev := <-ch:
		if ev.Action != "version" || ev.Rel != "dv.txt" {
			t.Fatalf("delete 事件内容不符: %+v", ev)
		}
		if ev.Size != 0 {
			t.Fatalf("delete 事件 size 应=0（删除非内容变更）, got %d", ev.Size)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delete 版本后未收到 version 事件")
	}
}
