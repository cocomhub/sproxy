// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "context"

// SpanContext is the minimal cross-request tracing identity. It carries the W3C
// trace id and span id of the current span, and is what the server middleware
// (task 2+) reads to propagate trace context across requests.
type SpanContext struct {
	TraceID string
	SpanID  string
}

// SpanContextKey is the context.Context key used to store a SpanContext.
// It is exported so external packages (e.g. the server middleware) can read
// and write the same value through context.WithValue / ctx.Value.
type SpanContextKey struct{}

// FromContext returns the SpanContext stored in ctx, if any.
func FromContext(ctx context.Context) (SpanContext, bool) {
	sc, ok := ctx.Value(SpanContextKey{}).(SpanContext)
	return sc, ok
}
