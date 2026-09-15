// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "context"

// Tracer is the minimum tracing interface. The built-in implementation is
// slogTracer; ext/otel wraps OpenTelemetry.
type Tracer interface {
	// StartSpan starts a new child span. It returns a context carrying the
	// current SpanContext (via SpanContextKey) plus the legacy Span (via
	// legacyContextKey), and a function that ends the span (typically
	// deferred).
	StartSpan(ctx context.Context, name string) (context.Context, func())
	// Inject writes the current span's W3C traceparent into the carrier.
	Inject(ctx context.Context, carrier Carrier)
}

// New creates the default slog-backed Tracer.
//
// 级别：span 结束行以 **Debug** 记录 ⇒ 默认 Info 级 handler 下静默（此前为 Info，SDK
// 每请求一行；见 docs/architecture.md「客户端追踪（`pkg/client` + `pkg/telemetry`）」小节）。
// 静默 ≠ 关闭：span 照常建立、traceparent 照旧注入请求，服务端 requestlog 仍能关联链路。
// 要看到 span 行：把 handler 级别调到 Debug（服务端/CLI 即 log_level: debug 或 `-v`），
// 或用 WithLogger 注入一个 Debug 级 logger。
func New(opts ...Option) Tracer {
	var o tracerOptions
	for _, opt := range opts {
		opt(&o)
	}
	return newSlogTracer(o.logger)
}

// Nop 返回一个真正什么都不做的 Tracer：StartSpan 原样返回入参 ctx 与空 end 函数、
// Inject 不写 traceparent——即**完全关闭追踪**（既无日志也无 traceparent 透传），
// 客户端 WithTracer(nil) 走这条。
//
// 若只是想要「静默但仍透传 traceparent」（默认 Info 级下 New() 的行为），请直接用 New()，
// 不要把 Nop 当「静默 tracer」使用。
func Nop() Tracer { return nopTracer{} }

// nopTracer 是 Nop() 的实现，见其注释。
type nopTracer struct{}

func (nopTracer) StartSpan(ctx context.Context, _ string) (context.Context, func()) {
	return ctx, func() {}
}

func (nopTracer) Inject(context.Context, Carrier) {}
