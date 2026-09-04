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
func New() Tracer {
	return newSlogTracer()
}
