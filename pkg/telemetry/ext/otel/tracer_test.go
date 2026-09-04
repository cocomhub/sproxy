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

func TestStartSpan_End_NoPanic(t *testing.T) {
	tr := newTestTracer(t)
	ctx, end := tr.StartSpan(context.Background(), "op")
	end()
	_ = ctx
}

func TestStartSpan_ReturnsActiveSpan(t *testing.T) {
	tr := newTestTracer(t)
	ctx, end := tr.StartSpan(context.Background(), "op")
	defer end()
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() || !span.SpanContext().IsValid() {
		t.Fatalf("expected a valid recording span in returned ctx, got %+v", span.SpanContext())
	}
}

func TestInject_TraceparentHeader(t *testing.T) {
	tr := newTestTracer(t)
	ctx, end := tr.StartSpan(context.Background(), "op")
	defer end()

	var c mapCarrier = map[string]string{}
	tr.Inject(ctx, c)

	tp := c["traceparent"]
	if !strings.HasPrefix(tp, "00-") {
		t.Fatalf("traceparent = %q, want 00- prefix", tp)
	}
}

// TestStartSpan_WritesCoreSpanContext 的装配路径覆盖见 provider_test.go
// （NewProvider → Tracer → StartSpan）；此处 Tracer 仅直接包装 otel tracer，
// 核心 SpanContext 写入行为由 provider_test 承接，避免重复声明。

// newTestTracer 构建一个由 sdktrace.NewTracerProvider 支撑的测试 Tracer。
func newTestTracer(t *testing.T) *Tracer {
	t.Helper()
	return New(sdktrace.NewTracerProvider().Tracer("test"))
}

// mapCarrier 是 testutil 的 map 版 Carrier（适配核心 telemetry.Carrier）。
type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }
