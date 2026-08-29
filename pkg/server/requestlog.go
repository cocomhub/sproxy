// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
)

// requestLogMiddleware 记录每个 HTTP 请求的收到与结束，并注入 trace_id/span_id 到 ctx。
// 解析 Traceparent（00-<32hex trace>-<16hex span>-01）：继承 trace_id，生成新的 span_id 作为子 span；
// 无 header 时自生成。响应头回显 traceparent。
func (h *Handlers) requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if h.logger == nil {
			h.logger = slog.Default()
		}
		traceID, _, ok := tracing.ParseTraceparent(r.Header.Get("Traceparent"))
		if !ok {
			traceID = tracing.TraceID()
		}
		spanID := tracing.SpanID()
		sc := tracing.SpanContext{TraceID: traceID, SpanID: spanID}
		ctx := context.WithValue(r.Context(), tracing.SpanContextKey{}, sc)
		r = r.WithContext(ctx)
		w.Header().Set("Traceparent", tracing.NewTraceparent(traceID, spanID))

		// 不显式传 trace_id/span_id：ctx 已带 SpanContext，日志经 WithContextHandler
		// 自动注入，避免同一行出现两对 trace_id/span_id 破坏日志解析。
		h.logger.DebugContext(ctx, "收到请求", "method", r.Method, "path", r.URL.Path,
			"remote_addr", r.RemoteAddr)

		mw := newMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)

		h.logger.InfoContext(ctx, "请求完成", "method", r.Method, "path", r.URL.Path,
			"status", mw.statusCode, "duration", time.Since(start).String())
	})
}
