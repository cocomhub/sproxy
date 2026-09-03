// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"log/slog"
)

// WithContextHandler returns a slog.Handler wrapping. It reads the SpanContext
// from ctx and, if present, automatically adds trace_id/span_id attrs to every
// record. This makes all InfoContext(ctx, ...) logs carry the chain IDs.
func WithContextHandler(inner slog.Handler) slog.Handler {
	return &contextHandler{inner: inner}
}

type contextHandler struct {
	inner slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc, ok := ctx.Value(SpanContextKey{}).(SpanContext); ok {
		if sc.TraceID != "" {
			r.AddAttrs(slog.String("trace_id", sc.TraceID))
		}
		if sc.SpanID != "" {
			r.AddAttrs(slog.String("span_id", sc.SpanID))
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{h.inner.WithAttrs(attrs)}
}
func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{h.inner.WithGroup(name)}
}
