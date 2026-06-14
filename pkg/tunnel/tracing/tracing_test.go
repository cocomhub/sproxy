// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
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
		ctx := context.Background()

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
		ctx := context.Background()

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
		ctx := context.Background()

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
	ctx := context.Background()

	_, end := tracer.StartSpan(ctx, "safe-end")
	end()
	end() // should not panic
}

func TestTracerTagsAppearInChildSpan(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := context.Background()

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
	ctx := context.Background()

	ctx, end := tracer.StartSpan(ctx, "op")
	end()
	ctx = WithTag(ctx, "after", "end")
	_ = ctx
}

func TestTracerDoubleEndProducesWarning(t *testing.T) {
	output := captureLog(t, func() {
		tracer := New()
		ctx := context.Background()

		_, end := tracer.StartSpan(ctx, "op")
		end()
		end() // second end should log a warning
	})

	if !hasLogLine(t, output, "already ended") {
		t.Errorf("expected warning 'already ended' on double end, got: %s", output)
	}
}
