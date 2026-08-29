// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(old)
	fn()
	return buf.String()
}

func hasLogLine(t *testing.T, output, substr string) bool {
	t.Helper()
	return strings.Contains(output, substr)
}

func TestTracerStartEnd(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := t.Context()

		_, end := tracer.StartSpan(ctx, "test-operation")
		end()
	})

	if !hasLogLine(t, output, "test-operation") {
		t.Errorf("expected log output to contain operation name 'test-operation', got: %s", output)
	}
	if !hasLogLine(t, output, "[trace") {
		t.Errorf("expected log output to contain trace marker '[trace', got: %s", output)
	}
}

func TestTracerNestedSpans(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := t.Context()

		ctx, endParent := tracer.StartSpan(ctx, "parent")
		_, endChild := tracer.StartSpan(ctx, "child")
		endChild()
		endParent()
	})

	if !hasLogLine(t, output, "parent") {
		t.Errorf("expected log to contain 'parent', got: %s", output)
	}
	if !hasLogLine(t, output, "child") {
		t.Errorf("expected log to contain 'child', got: %s", output)
	}
}

func TestTracerWithTag(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := t.Context()

		ctx, end := tracer.StartSpan(ctx, "tagged-op")
		ctx = WithTag(ctx, "env", "test")
		ctx = WithTag(ctx, "user", "alice")
		_ = ctx
		end()
	})

	if !hasLogLine(t, output, "env=test") {
		t.Errorf("expected log to contain 'env=test', got: %s", output)
	}
	if !hasLogLine(t, output, "user=alice") {
		t.Errorf("expected log to contain 'user=alice', got: %s", output)
	}
}

func TestTracerEndTwiceSafe(t *testing.T) {
	tracer := New()
	ctx := t.Context()

	_, end := tracer.StartSpan(ctx, "safe-end")
	end()
	end() // should not panic
}

func TestTracerTagsAppearInChildSpan(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := t.Context()

		ctx, _ = tracer.StartSpan(ctx, "outer")
		ctx = WithTag(ctx, "region", "us-east")
		_, end := tracer.StartSpan(ctx, "inner")
		end()
	})

	if !hasLogLine(t, output, "region=us-east") {
		t.Errorf("expected child span log to contain inherited tag 'region=us-east', got: %s", output)
	}
}

func TestTracerWithTagAfterEnd(t *testing.T) {
	// WithTag after ends should not panic
	tracer := New()
	ctx := t.Context()

	ctx, end := tracer.StartSpan(ctx, "op")
	end()
	ctx = WithTag(ctx, "after", "end")
	_ = ctx
}

func TestTracerDoubleEndProducesWarning(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := t.Context()

		_, end := tracer.StartSpan(ctx, "op")
		end()
		end() // second end should log a warning
	})

	if !hasLogLine(t, output, "already ended") {
		t.Errorf("expected warning 'already ended' on double end, got: %s", output)
	}
}

// --- W3C traceparent support (task 1) ---

func TestTraceID_Length32(t *testing.T) {
	id := TraceID()
	if len(id) != 32 {
		t.Fatalf("TraceID() = %q (len %d), want 32", id, len(id))
	}
	if _, _, ok := ParseTraceparent("00-" + id + "-" + spanIDPlaceholder + "-01"); !ok {
		t.Fatalf("generated TraceID not hex: %q", id)
	}
}

func TestSpanID_Length16(t *testing.T) {
	id := SpanID()
	if len(id) != 16 {
		t.Fatalf("SpanID() = %q (len %d), want 16", id, len(id))
	}
}

func TestParseTraceparent_Valid(t *testing.T) {
	traceID, spanID, ok := ParseTraceparent("00-0123456789abcdef0123456789abcdef-abcd1234abcd1234-01")
	if !ok || traceID != "0123456789abcdef0123456789abcdef" || spanID != "abcd1234abcd1234" {
		t.Fatalf("ParseTraceparent = %q %q %v, want valid", traceID, spanID, ok)
	}
}

func TestParseTraceparent_Invalid(t *testing.T) {
	for _, s := range []string{"", "00-abc-ef-01", "01-0123456789abcdef0123456789abcdef-abcd1234abcd1234-01", "00-zz123456789abcdef0123456789abcdef-abcd1234abcd1234-01"} {
		if _, _, ok := ParseTraceparent(s); ok {
			t.Fatalf("ParseTraceparent(%q) = ok, want invalid", s)
		}
	}
}

func TestTracer_StartSpan_InheritsTraceID(t *testing.T) {
	tr := New()
	ctx, end := tr.StartSpan(context.Background(), "root")
	sc, _ := FromContext(ctx)
	if len(sc.TraceID) != 32 || len(sc.SpanID) != 16 {
		t.Fatalf("root SpanContext = %+v, want 32/16", sc)
	}
	ctx2, end2 := tr.StartSpan(ctx, "child")
	sc2, _ := FromContext(ctx2)
	if sc2.TraceID != sc.TraceID {
		t.Fatalf("child trace %q != parent %q", sc2.TraceID, sc.TraceID)
	}
	if sc2.SpanID == sc.SpanID {
		t.Fatalf("child span %q == parent span %q, want different", sc2.SpanID, sc.SpanID)
	}
	end2()
	end()
}

var spanIDPlaceholder = "abcd1234abcd1234"

func TestTracer_Inject(t *testing.T) {
	tr := New()
	ctx, end := tr.StartSpan(context.Background(), "root")
	defer end()
	sc, _ := FromContext(ctx)
	var c mapCarrier = map[string]string{}
	tr.Inject(ctx, c)
	want := "00-" + sc.TraceID + "-" + sc.SpanID + "-01"
	if c["traceparent"] != want {
		t.Fatalf("traceparent = %q, want %q", c["traceparent"], want)
	}
}

type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }

func TestContextHandler_AddsTraceSpan(t *testing.T) {
	var got string
	inner := slog.NewTextHandler(&bufWriter{&got}, nil)
	h := WithContextHandler(inner)
	ctx := context.WithValue(context.Background(), SpanContextKey{}, SpanContext{TraceID: "t", SpanID: "s"})
	_ = h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0))
	if !strings.Contains(got, "trace_id=t") || !strings.Contains(got, "span_id=s") {
		t.Fatalf("output = %q, want trace_id/span_id", got)
	}
}

func TestContextHandler_NoSpan_NoAttrs(t *testing.T) {
	var got string
	inner := slog.NewTextHandler(&bufWriter{&got}, nil)
	h := WithContextHandler(inner)
	_ = h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0))
	if strings.Contains(got, "trace_id=") || strings.Contains(got, "span_id=") {
		t.Fatalf("output = %q, non-empty trace_id/span_id without context", got)
	}
}

// bufWriter is a test helper that appends every write to the referenced string.
type bufWriter struct{ s *string }

func (b *bufWriter) Write(p []byte) (n int, err error) { *b.s += string(p); return len(p), nil }
