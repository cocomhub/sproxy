// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/telemetry"
)

func doThroughMiddleware(h *Handlers, method, path, traceparent string) (*httptest.ResponseRecorder, *bytes.Buffer) {
	var out bytes.Buffer
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证 ctx 里已有 SpanContext
		if _, ok := telemetry.FromContext(r.Context()); !ok {
			tFatalfAlive("no SpanContext in ctx")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(method, path, nil)
	if traceparent != "" {
		req.Header.Set("Traceparent", traceparent)
	}
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	return rr, &out
}

func tFatalfAlive(format string, args ...any) { panic(strings.TrimSpace("alive:" + format)) }

func TestRequestLog_NoHeader(t *testing.T) {
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rr, _ := doThroughMiddleware(h, "GET", "/api/files", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	got := rr.Header().Get("Traceparent")
	if _, _, ok := telemetry.ParseTraceparent(got); !ok {
		t.Fatalf("response Traceparent = %q, invalid", got)
	}
}

func TestRequestLog_NoHeader_SelfGeneratedHasSpanContext(t *testing.T) {
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen telemetry.SpanContext
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, _ := telemetry.FromContext(r.Context())
		seen = sc
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if len(seen.TraceID) != 32 || len(seen.SpanID) != 16 {
		t.Fatalf("SpanContext = %+v, want 32/16", seen)
	}
}

func TestRequestLog_InheritTraceID(t *testing.T) {
	parent := telemetry.NewTraceparent(strings.Repeat("a", 32), strings.Repeat("b", 16))
	var seen telemetry.SpanContext
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = telemetry.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Traceparent", parent)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if seen.TraceID != strings.Repeat("a", 32) || len(seen.SpanID) != 16 || seen.SpanID == strings.Repeat("b", 16) {
		t.Fatalf("SpanContext = %+v, want inherited trace + new span", seen)
	}
}

// TestRequestLog_NoDuplicateTraceAttrs 验证"收到请求/请求完成"日志经 WithContextHandler
// 自动注入 trace_id/span_id 时不会重复（生产 logger 已用 WithContextHandler 包装，
// 中间件 ctx 又带 SpanContext，显式传参会导致每行出现两对 trace_id/span_id）。
func TestRequestLog_NoDuplicateTraceAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := &Handlers{}
	h.logger = slog.New(telemetry.WithContextHandler(
		slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/api/files", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected request log output")
	}
	for line := range strings.SplitSeq(out, "\n") {
		if n := strings.Count(line, "trace_id="); n > 1 {
			t.Fatalf("duplicate trace_id (%d) in line: %s", n, line)
		}
		if n := strings.Count(line, "span_id="); n > 1 {
			t.Fatalf("duplicate span_id (%d) in line: %s", n, line)
		}
	}
}

// fakeTracer 是测试用 telemetry.Tracer：写入固定 SpanContext，记录 end 调用。
type fakeTracer struct {
	started int
	ended   int
}

func (f *fakeTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	f.started++
	sc := telemetry.SpanContext{TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16)}
	ctx = context.WithValue(ctx, telemetry.SpanContextKey{}, sc)
	return ctx, func() { f.ended++ }
}

func (f *fakeTracer) Inject(ctx context.Context, c telemetry.Carrier) {
	if sc, ok := telemetry.FromContext(ctx); ok {
		c.Set("traceparent", telemetry.NewTraceparent(sc.TraceID, sc.SpanID))
	}
}

// TestRequestLog_TracerBridge 验证 requestlog 中间件接通 telemetry.Tracer 后：
//  1. 到达 handler 的 ctx 携带 fake tracer 写入的 SpanContext（TraceID/SpanID）；
//  2. 响应头 Traceparent 从该 SpanContext 回显（32/16 hex）；
//  3. end() 在请求完成后被调用一次（span 生命周期覆盖请求处理）。
func TestRequestLog_TracerBridge(t *testing.T) {
	ft := &fakeTracer{}
	h := &Handlers{tracer: ft}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen telemetry.SpanContext
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = telemetry.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest("GET", "/api/files", nil)
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)

	if seen.TraceID != strings.Repeat("1", 32) || seen.SpanID != strings.Repeat("2", 16) {
		t.Fatalf("handler 看到的 SpanContext = %+v, want fake tracer 写入的 32/16", seen)
	}
	got := rr.Header().Get("Traceparent")
	if got != telemetry.NewTraceparent(strings.Repeat("1", 32), strings.Repeat("2", 16)) {
		t.Fatalf("响应 Traceparent = %q, want fake tracer 的 trace/span id", got)
	}
	if ft.started != 1 || ft.ended != 1 {
		t.Fatalf("fake tracer 调用次数 = started=%d ended=%d, want 1/1（span 生命周期覆盖请求）", ft.started, ft.ended)
	}
}

// TestRequestLog_TracerNil_BehavesAsBefore 验证 Tracer=nil（默认）时
// requestlog 行为不变：自生成 SpanContext（32/16 hex），响应投 Traceparent。
func TestRequestLog_TracerNil_BehavesAsBefore(t *testing.T) {
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rr, _ := doThroughMiddleware(h, "GET", "/api/files", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	got := rr.Header().Get("Traceparent")
	if _, _, ok := telemetry.ParseTraceparent(got); !ok {
		t.Fatalf("响应 Traceparent = %q, invalid（Tracer=nil 应保持自生成行为）", got)
	}
}
