// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
)

// contextKey is used to store the current span in a context.Context.
type contextKey struct{}

// slogTracer implements Tracer with log/slog output.
type slogTracer struct {
	mu    sync.Mutex
	spans []*Span
	depth int
}

func newSlogTracer() *slogTracer {
	return &slogTracer{}
}

func (t *slogTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	traceID := hexID()
	parentID := ""
	tags := make(map[string]string)

	if parent := spanFromContext(ctx); parent != nil {
		traceID = parent.TraceID
		parentID = parent.SpanID
		maps.Copy(tags, parent.Tags)
	}

	span := &Span{
		TraceID:   traceID,
		SpanID:    hexID(),
		ParentID:  parentID,
		Name:      name,
		StartTime: time.Now(),
		Tags:      tags,
	}

	newCtx := context.WithValue(ctx, contextKey{}, span)

	t.spans = append(t.spans, span)
	t.depth++

	depth := t.depth

	return newCtx, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if span.ended {
			slog.Warn("span already ended")
			return
		}
		span.ended = true
		span.Duration = time.Since(span.StartTime.(time.Time))

		indent := ""
		if depth > 1 {
			indent = strings.Repeat("  ", depth-1)
		}

		attrs := slog.String("trace_id", span.TraceID)
		if len(span.Tags) > 0 {
			attrs = slog.Group("tags", tagsToAttrs(span.Tags)...)
		}

		slog.Info(fmt.Sprintf("%s[trace %s] %s %v", indent, span.TraceID, span.Name, span.Duration), attrs)
	}
}

// WithTag attaches a key-value tag to the span stored in the context.
// If no span is found, the tag is silently dropped.
func WithTag(ctx context.Context, key, value string) context.Context {
	span := spanFromContext(ctx)
	if span == nil {
		return ctx
	}
	span.Tags[key] = value
	return ctx
}

func hexID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%016x", b)
}

func spanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(contextKey{}).(*Span)
	return s
}

func tagsToAttrs(tags map[string]string) []any {
	attrs := make([]any, 0, len(tags)*2)
	for k, v := range tags {
		attrs = append(attrs, slog.String(k, v))
	}
	return attrs
}
