// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oteltracing

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }

func testTracer() *Tracer {
	return New(sdktrace.NewTracerProvider().Tracer("test"))
}

func TestStartSpan_End_NoPanic(t *testing.T) {
	tr := testTracer()
	ctx, end := tr.StartSpan(context.Background(), "op")
	end()
	_ = ctx
}

func TestStartSpan_ReturnsActiveSpan(t *testing.T) {
	tr := testTracer()
	ctx, end := tr.StartSpan(context.Background(), "op")
	defer end()
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() || !span.SpanContext().IsValid() {
		t.Fatalf("expected a valid recording span in returned ctx, got %+v", span.SpanContext())
	}
}

func TestInject_TraceparentHeader(t *testing.T) {
	tr := testTracer()
	ctx, end := tr.StartSpan(context.Background(), "op")
	defer end()

	var c mapCarrier = map[string]string{}
	tr.Inject(ctx, c)

	tp := c["traceparent"]
	if !strings.HasPrefix(tp, "00-") {
		t.Fatalf("traceparent = %q, want 00- prefix", tp)
	}
}
