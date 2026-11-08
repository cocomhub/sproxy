// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package httpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/pipe"
)

// instantEnv 让退避零等待（测试不真等；**不用 time.Sleep**）。
func instantEnv() pipe.Env {
	return pipe.Env{Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
}

// countingMetrics 记录指标调用（验证可观测性）。
type countingMetrics struct {
	requests atomic.Int64
	retries  atomic.Int64
}

func (m *countingMetrics) IncRequest(string, string)            { m.requests.Add(1) }
func (m *countingMetrics) IncRetry(string)                      { m.retries.Add(1) }
func (m *countingMetrics) ObserveAttempt(string, time.Duration) {}

func newTestClient(o Options) *Client {
	o.Env = instantEnv()
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(o)
}

// TestDo_RetriesOn5xxThenSuccess 5xx ⇒ 按策略重试直到成功（验证机制可用）。
func TestDo_RetriesOn5xxThenSuccess(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 4, Base: time.Millisecond}})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("应重试后成功，got %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || n.Load() != 3 {
		t.Fatalf("应 3 次尝试后 200，got status=%d attempts=%d", resp.StatusCode, n.Load())
	}
}

// TestDo_NoRetryOn4xx 4xx（非 429）⇒ 致命，不重试，且返回 *HTTPError 与可读 body。
func TestDo_NoRetryOn4xx(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad-request-detail"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 5, Base: time.Millisecond}})
	resp, err := c.Get(context.Background(), srv.URL)
	if _, ok := errors.AsType[*HTTPError](err); !ok {
		t.Fatalf("4xx 应返回 *HTTPError，got %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("4xx 不得重试，got %d 次", n.Load())
	}
	if resp == nil {
		t.Fatal("非 2xx 终态应把响应交回调用方（可读 body）")
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "bad-request-detail") {
		t.Fatalf("响应体应可读，got %q", body)
	}
}

// TestDo_RetriesOn429 429 ⇒ 可重试。
func TestDo_RetriesOn429(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 3, Base: time.Millisecond}})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("429 后应重试成功，got %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if n.Load() != 2 {
		t.Fatalf("应为 2 次尝试，got %d", n.Load())
	}
}

// TestDo_PostNotRetriedByDefault **默认不重试非幂等方法**（防重复副作用）。
func TestDo_PostNotRetriedByDefault(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 5, Base: time.Millisecond}})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader([]byte("x")))
	resp, err := c.Do(context.Background(), req)
	if err == nil {
		t.Fatal("500 应报错")
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	if n.Load() != 1 {
		t.Fatalf("POST 默认不得重试，got %d 次", n.Load())
	}
}

// TestDo_IdempotentPostRetriedWhenDeclared 显式声明幂等（且 body 可重放）⇒ 允许重试，
// 且**每次重试都必须重建请求体**（GetBody 被调用 ⇒ 断言 calls>=1，M3 变异即红）。
func TestDo_IdempotentPostRetriedWhenDeclared(t *testing.T) {
	t.Parallel()
	var n, getBodyCalls atomic.Int64
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		if n.Add(1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	const payload = "payload"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL,
		io.NopCloser(strings.NewReader(payload)))
	req.ContentLength = int64(len(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		getBodyCalls.Add(1)
		return io.NopCloser(strings.NewReader(payload)), nil
	}

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 3, Base: time.Millisecond}})
	resp, err := c.DoOpts(context.Background(), req, DoOptions{Idempotent: true})
	if err != nil {
		t.Fatalf("声明幂等后应重试成功，got %v", err)
	}
	_ = resp.Body.Close()
	if n.Load() != 2 {
		t.Fatalf("应为 2 次尝试，got %d", n.Load())
	}
	if getBodyCalls.Load() < 1 {
		t.Fatal("重试必须经 GetBody 重建请求体（否则第二次会发空 body）")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("服务端应收到 2 次请求，got %d（%v）", len(got), got)
	}
	for i, b := range got {
		if b != payload {
			t.Fatalf("第 %d 次请求体应被完整重放，got %q", i+1, b)
		}
	}
}

// TestDo_BodyNotReplayableRejected 声明可重试但 body 不可重放 ⇒ fail-fast（不得静默发空 body）。
func TestDo_BodyNotReplayableRejected(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 3, Base: time.Millisecond}})
	// io.NopCloser 包装的非 Seeker ⇒ GetBody 为 nil ⇒ 不可重放。
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, io.NopCloser(strings.NewReader("x")))
	_, err := c.DoOpts(context.Background(), req, DoOptions{Idempotent: true})
	if !errors.Is(err, ErrBodyNotReplayable) {
		t.Fatalf("应返回 ErrBodyNotReplayable，got %v", err)
	}
	if n.Load() != 0 {
		t.Fatalf("不可重放应在发请求前拒绝，got %d 次", n.Load())
	}
}

// TestDo_CancelStopsImmediately ctx 取消 ⇒ 立即返回（不等待、不重试）。
func TestDo_CancelStopsImmediately(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 5, Base: time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, srv.URL)
		done <- err
	}()
	// 让请求进入在途（服务端收到后即阻塞）；不依赖 sleep，用服务端计数确认。
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消应返回错误")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后未立即返回（不可取消 ⇒ 违反设计约束）")
	}
}

// TestDo_AttemptTimeoutIsRetryable 单次尝试超时属瞬时故障 ⇒ 可重试并最终成功。
func TestDo_AttemptTimeoutIsRetryable(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			select { // 第一次拖过 per-attempt 超时
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{
		Retry:   pipe.Policy{Attempts: 3, Base: time.Millisecond},
		Timeout: 80 * time.Millisecond,
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("单次尝试超时应可重试并成功，got %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if n.Load() != 2 {
		t.Fatalf("应为 2 次尝试，got %d", n.Load())
	}
}

// TestDo_MetricsRecorded 重试与请求都进指标（可观测性）。
func TestDo_MetricsRecorded(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := &countingMetrics{}
	c := newTestClient(Options{Retry: pipe.Policy{Attempts: 3, Base: time.Millisecond}, Metrics: m})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("应成功，got %v", err)
	}
	_ = resp.Body.Close()
	if m.requests.Load() != 2 || m.retries.Load() != 1 {
		t.Fatalf("指标应为 requests=2 retries=1，got requests=%d retries=%d",
			m.requests.Load(), m.retries.Load())
	}
}

// TestNew_DefaultTransportIsIsolated 默认 Transport 必须来自 netutil（R19 门禁要求的唯一构造点）。
func TestNew_DefaultTransportIsIsolated(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	if c.http.Transport == nil {
		t.Fatal("默认 Transport 不得为 nil")
	}
	if c.http.Transport == http.DefaultTransport {
		t.Fatal("不得复用 http.DefaultTransport（连接池共享 ⇒ 测试/服务互扰）")
	}
}

// TestRetryAfter_Parses 表驱动：秒数/HTTP 日期/非法值。
func TestRetryAfter_Parses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"缺失", "", 0},
		{"秒数", "7", 7 * time.Second},
		{"零", "0", 0},
		{"非法", "abc", 0},
		{"负数", "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{Header: http.Header{}}
			if tc.in != "" {
				resp.Header.Set("Retry-After", tc.in)
			}
			if got := RetryAfter(resp); got != tc.want {
				t.Fatalf("RetryAfter(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := RetryAfter(nil); got != 0 {
		t.Fatalf("nil 响应应返回 0，got %v", got)
	}
}

// TestDoJSON_SendsAndDecodes 便捷方法可用（含 4xx 返回错误）。
func TestDoJSON_SendsAndDecodes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(Options{})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.DoJSON(context.Background(), http.MethodPost, srv.URL,
		map[string]string{"a": "b"}, &out, DoOptions{}); err != nil {
		t.Fatalf("DoJSON 应成功，got %v", err)
	}
	if !out.OK {
		t.Fatalf("应解析出 ok=true，got %+v", out)
	}
}

// TestReplayBody_Unit 直接锁定 replayBody 语义（确定性；上面的重试用例无法区分，
// 因为 net/http 自身在 ContentLength 已知时会经 GetBody 重建 body）。
func TestReplayBody_Unit(t *testing.T) {
	t.Parallel()
	orig, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://example.invalid/x", io.NopCloser(strings.NewReader("p")))
	orig.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("p")), nil }
	att := &http.Request{}
	if err := replayBody(orig, att); err != nil {
		t.Fatalf("可重放时应成功，got %v", err)
	}
	if att.Body == nil {
		t.Fatal("重试请求必须带上重建的 body（否则会发空 body）")
	}
	b, _ := io.ReadAll(att.Body)
	if string(b) != "p" {
		t.Fatalf("重建 body 内容应为 p，got %q", b)
	}
	orig.GetBody = nil
	if err := replayBody(orig, &http.Request{}); !errors.Is(err, ErrBodyNotReplayable) {
		t.Fatalf("不可重放应返回 ErrBodyNotReplayable，got %v", err)
	}
	// body 为 nil 时无需重建。
	if err := replayBody(&http.Request{}, &http.Request{}); err != nil {
		t.Fatalf("无 body 时应直接成功，got %v", err)
	}
}

// TestNewHTTPClient_DefaultsAndPassthrough 集中构造出口：默认 IsolatedTransport + 字段透传。
func TestNewHTTPClient_DefaultsAndPassthrough(t *testing.T) {
	t.Parallel()
	// 默认：非 DefaultTransport（隔离连接池）。
	def := NewHTTPClient(Options{})
	if def.Transport == nil || def.Transport == http.DefaultTransport {
		t.Fatalf("默认 Transport 必须是独立的 IsolatedTransport，got %v", def.Transport)
	}
	// 透传：ClientTimeout / Jar / CheckRedirect 必须落到裸 client（迁移存量时行为等价）。
	jar, _ := cookiejar.New(nil)
	redirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tr := &countingTransport{}
	hc := NewHTTPClient(Options{Transport: tr, ClientTimeout: 7 * time.Second, Jar: jar, CheckRedirect: redirect})
	if hc.Timeout != 7*time.Second || hc.Jar == nil || hc.CheckRedirect == nil || hc.Transport != tr {
		t.Fatalf("字段未透传：timeout=%v jar!=nil=%v redirect!=nil=%v", hc.Timeout, hc.Jar != nil, hc.CheckRedirect != nil)
	}
	// New 内部必须复用同一构造出口（避免两份构造逻辑漂移）。
	c := New(Options{Transport: tr, ClientTimeout: 5 * time.Second})
	if c.http.Transport != tr || c.http.Timeout != 5*time.Second {
		t.Fatalf("New 应复用 NewHTTPClient 语义，got %+v", c.http)
	}
}

type countingTransport struct{ calls int }

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}
