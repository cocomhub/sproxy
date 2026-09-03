// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/telemetry"
)

// actorCarrier 允许认证中间件把已认证的 actor 写入响应包装器，
// 供 requestLogMiddleware 在请求完成后记录 actor 字段（认证发生在 requestLog
// 之后，ctx 不回传，故经 ResponseWriter 携带回调用方）。
type actorCarrier interface {
	setActor(string)
	actor() string
}

// setResponseActor 把已认证 actor 写入响应包装器（若其实现 actorCarrier）。
// 未实现时静默忽略（如 httptest.ResponseRecorder），不影响认证流程。
func setResponseActor(w http.ResponseWriter, actor string) {
	if c, ok := w.(actorCarrier); ok {
		c.setActor(actor)
	}
}

// requestLogMiddleware 记录每个 HTTP 请求的收到与结束，并注入 trace_id/span_id 到 ctx。
// 解析 Traceparent（00-<32hex trace>-<16hex span>-01）：继承 trace_id，生成新的 span_id 作为子 span；
// 无 header 时自生成。响应头回显 traceparent。
//
// 可选的 telemetry.Tracer（h.tracer，RegisterRoutesOpts.Tracer 注入）接通后，
// 本中间件为该请求建立真实 OTel span：trace/span id 由 tracer 生成并写入
// SpanContextKey（tracer 实现负责写 core.SpanContext，如 ext/otel 适配器），
// 响应头回显从 ctx 读回的 id。两条路径（nil/非 nil tracer）id 来源一致，
// WithContextHandler 均能从 ctx 的 SpanContext 注入 trace_id/span_id 日志。
// 请求完成日志会带上 actor 字段（认证成功后由 authMiddleware 写入响应包装器），
// 未认证请求省略 actor 字段。
func (h *Handlers) requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if h.logger == nil {
			h.logger = slog.Default()
		}

		ctx := r.Context()
		if h.tracer != nil {
			// OTel tracer 接通：span 生命周期覆盖整个请求处理，ctx 携带
			// core.SpanContext（由 tracer 的 StartSpan 写入 SpanContextKey）。
			var end func()
			ctx, end = h.tracer.StartSpan(ctx, r.Method+" "+r.URL.Path)
			defer end()
			r = r.WithContext(ctx)
			if sc, ok := telemetry.FromContext(ctx); ok {
				w.Header().Set("Traceparent", telemetry.NewTraceparent(sc.TraceID, sc.SpanID))
			}
		} else {
			// 无 tracer：自生成（继承上游 trace_id），保持既有行为。
			traceID, _, ok := telemetry.ParseTraceparent(r.Header.Get("Traceparent"))
			if !ok {
				traceID = telemetry.TraceID()
			}
			spanID := telemetry.SpanID()
			sc := telemetry.SpanContext{TraceID: traceID, SpanID: spanID}
			ctx = context.WithValue(ctx, telemetry.SpanContextKey{}, sc)
			r = r.WithContext(ctx)
			w.Header().Set("Traceparent", telemetry.NewTraceparent(traceID, spanID))
		}

		// 不显式传 trace_id/span_id：ctx 已带 SpanContext，日志经 WithContextHandler
		// 自动注入，避免同一行出现两对 trace_id/span_id 破坏日志解析。
		h.logger.DebugContext(ctx, "收到请求", "method", r.Method, "path", r.URL.Path,
			"remote_addr", r.RemoteAddr)

		mw := newMetricsResponseWriter(w)
		next.ServeHTTP(mw, r)

		// actor 在认证中间件（requestLog 之后的 handler 链）写入响应包装器；
		// 隧道内层请求（无 authMiddleware）经 ctx 携带 actor。两者互补：
		// 外层用 mw.actor()（auth 写入），内层回退 ActorFrom(r.Context())。
		// 未认证请求为空串，省略字段以保持日志简洁（审查 M-1）。
		args := []any{"method", r.Method, "path", r.URL.Path,
			"status", mw.statusCode, "duration", time.Since(start).String()}
		a := mw.actor()
		if a == "" {
			a = ActorFrom(r.Context())
		}
		if a != "" {
			args = append(args, "actor", a)
		}
		h.logger.InfoContext(ctx, "请求完成", args...)
	})
}
