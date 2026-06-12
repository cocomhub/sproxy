// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import "context"

// Span represents a single tracing span.
type Span struct {
	TraceID   string
	SpanID    string
	ParentID  string
	Name      string
	StartTime any // time.Time; typed as any to keep stdlib-only
	Duration  any // time.Duration
	Tags      map[string]string
	ended     bool
}

// Tracer defines the minimum tracing interface.
// Built-in impl is slogTracer; ext/otel wraps OpenTelemetry.
type Tracer interface {
	// StartSpan starts a new child span. Returns a context carrying the span
	// and a function that ends it (typically deferred).
	StartSpan(ctx context.Context, name string) (context.Context, func())
}

// New creates a new default Tracer (slog-backed).
func New() Tracer {
	return newSlogTracer()
}
