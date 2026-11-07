// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyTransport 是可控故障注入的 RoundTripper：前 failTransport 次请求返回传输层
// 错误（TLS 握手超时），其余交给 base。calls 记录总尝试次数（含重试）。
type flakyTransport struct {
	calls         atomic.Int32
	failTransport int32
	base          http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.calls.Add(1) <= f.failTransport {
		return nil, tlsTimeoutErr{}
	}
	base := f.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// newAPITestServer 起一个 fake API 服务（返回固定 JSON），返回 server 与命中计数。
func newAPITestServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// TestAPI_DownloadLinkRetriesTransientHandshake 是用户实测故障的回归测试：
// 取 FETCH 直链首次 TLS 握手超时（净传输层瞬断）→ 重试后成功，不致命。
func TestAPI_DownloadLinkRetriesTransientHandshake(t *testing.T) {
	t.Parallel()
	srv, hits := newAPITestServer(t, http.StatusOK, `{"web_content_link":"https://cdn/dl"}`)
	rt := &flakyTransport{failTransport: 2}
	api := NewAPI(APIConfig{
		Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(),
		HTTPClient: &http.Client{Transport: rt}, RetryBackoff: time.Millisecond,
	}, nil)

	link, err := api.DownloadLink(context.Background(), "file-1")
	if err != nil {
		t.Fatalf("DownloadLink 应在瞬断重试后成功: %v", err)
	}
	if link != "https://cdn/dl" {
		t.Errorf("link = %q", link)
	}
	if got := rt.calls.Load(); got != 3 {
		t.Errorf("总尝试 = %d, want 3（1 次 + 2 次重试）", got)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("服务端命中 = %d, want 1（失败发生在握手前）", got)
	}
}

// TestAPI_GETRetries5xx 服务端 5xx（真实响应）→ 重试到成功。
func TestAPI_GETRetries5xx(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]any{"files": []any{}})
	}))
	defer srv.Close()
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(), HTTPClient: srv.Client(), RetryBackoff: time.Millisecond}, nil)

	if _, err := api.List(context.Background(), ""); err != nil {
		t.Fatalf("List 应在 503 重试后成功: %v", err)
	}
	if got := n.Load(); got != 3 {
		t.Errorf("服务端命中 = %d, want 3", got)
	}
}

// TestAPI_NoRetryOn403 鉴权/权限类 4xx 不重试（重试无意义且浪费配额）。
func TestAPI_NoRetryOn403(t *testing.T) {
	t.Parallel()
	srv, hits := newAPITestServer(t, http.StatusForbidden, `{"error":"forbidden"}`)
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(), HTTPClient: srv.Client(), RetryBackoff: time.Millisecond}, nil)

	if _, err := api.DownloadLink(context.Background(), "file-1"); err == nil {
		t.Fatal("err = nil, want 403 error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("服务端命中 = %d, want 1（403 不重试）", got)
	}
}

// TestAPI_POSTNotRetried 非幂等 POST 不重试（避免重放副作用）。
func TestAPI_POSTNotRetried(t *testing.T) {
	t.Parallel()
	srv, _ := newAPITestServer(t, http.StatusOK, `{"task_id":"t1"}`)
	rt := &flakyTransport{failTransport: 5} // 全部失败
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(), HTTPClient: &http.Client{Transport: rt}}, nil)

	if err := api.Delete(context.Background(), []string{"id-1"}); err == nil {
		t.Fatal("err = nil, want transport error")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Errorf("POST 尝试次数 = %d, want 1", got)
	}
}

// TestAPI_CtxCancelStopsRetry ctx 取消 → 立即返回 ctx 语义错误，不再重试。
func TestAPI_CtxCancelStopsRetry(t *testing.T) {
	t.Parallel()
	srv, _ := newAPITestServer(t, http.StatusOK, `{"web_content_link":"x"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &flakyTransport{failTransport: 5}
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(), HTTPClient: &http.Client{Transport: rt}}, nil)

	if _, err := api.DownloadLink(ctx, "file-1"); err == nil {
		t.Fatal("err = nil, want ctx error")
	}
	if got := rt.calls.Load(); got != 0 {
		t.Errorf("ctx 已取消不应发起请求: calls = %d", got)
	}
}

// TestAPI_RetryExhaustedReturnsLastError 重试耗尽 → 返回最后一次错误（含 what 上下文）。
func TestAPI_RetryExhaustedReturnsLastError(t *testing.T) {
	t.Parallel()
	srv, hits := newAPITestServer(t, http.StatusBadGateway, `{"error":"bad gateway"}`)
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, Logger: apiTestLogger(), HTTPClient: srv.Client(), RetryBackoff: time.Millisecond}, nil)

	_, err := api.DownloadLink(context.Background(), "file-1")
	if err == nil {
		t.Fatal("err = nil, want error")
	}
	if want := 1 + defaultRetryCount; int(hits.Load()) != want {
		t.Errorf("服务端命中 = %d, want %d", hits.Load(), want)
	}
	if got := err.Error(); !strings.Contains(got, fmt.Sprint(http.StatusBadGateway)) {
		t.Errorf("err = %q, want 含 502", got)
	}
}

// apiTestLogger 返回丢弃日志的 logger（重试 WARN 不污染测试输出）。
func apiTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
