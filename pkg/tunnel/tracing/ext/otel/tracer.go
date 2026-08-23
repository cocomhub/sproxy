// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oteltracing adapts the core tracing.Tracer interface to
// OpenTelemetry, making the built-in slog tracer pluggable with a real
// OpenTelemetry tracer provider.
package oteltracing

import (
	"context"

	core "github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Tracer adapts the core tracing.Tracer interface to OpenTelemetry.
type Tracer struct {
	tracer     oteltrace.Tracer
	propagator propagation.TraceContext
}

// New wraps an OpenTelemetry tracer so it can be used wherever the core
// tracing.Tracer interface is expected.
func New(t oteltrace.Tracer) *Tracer {
	return &Tracer{tracer: t, propagator: propagation.TraceContext{}}
}

// StartSpan starts a new OpenTelemetry span and returns a context carrying the
// active span plus a function that ends it.
func (t *Tracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	ctx, span := t.tracer.Start(ctx, name)
	return ctx, func() { span.End() }
}

// Inject writes the current span's W3C traceparent header into carrier.
func (t *Tracer) Inject(ctx context.Context, carrier core.Carrier) {
	t.propagator.Inject(ctx, headerCarrier{carrier})
}

// headerCarrier adapts the stdlib-only core.Carrier to OpenTelemetry's
// propagation.TextMapCarrier.
type headerCarrier struct {
	c core.Carrier
}

func (h headerCarrier) Get(key string) string { return h.c.Get(key) }
func (h headerCarrier) Set(key, value string) { h.c.Set(key, value) }
func (h headerCarrier) Keys() []string        { return nil }
