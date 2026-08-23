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
