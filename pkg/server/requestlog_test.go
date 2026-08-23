// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
)

func doThroughMiddleware(h *Handlers, method, path, traceparent string) (*httptest.ResponseRecorder, *bytes.Buffer) {
	var out bytes.Buffer
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证 ctx 里已有 SpanContext
		if _, ok := tracing.FromContext(r.Context()); !ok {
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
	if _, _, ok := tracing.ParseTraceparent(got); !ok {
		t.Fatalf("response Traceparent = %q, invalid", got)
	}
}

func TestRequestLog_NoHeader_SelfGeneratedHasSpanContext(t *testing.T) {
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var seen tracing.SpanContext
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, _ := tracing.FromContext(r.Context())
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
	parent := tracing.NewTraceparent(strings.Repeat("a", 32), strings.Repeat("b", 16))
	var seen tracing.SpanContext
	h := &Handlers{}
	h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	wrapped := h.requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = tracing.FromContext(r.Context())
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
	h.logger = slog.New(tracing.WithContextHandler(
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
