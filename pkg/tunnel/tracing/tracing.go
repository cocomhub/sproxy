// Copyright 2026 The Cocomhub Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

// Package tracing provides a lightweight OpenTelemetry-like tracing skeleton
// built on standard log/slog, with no external dependencies. It can be replaced
// by the real OTel SDK later without changing caller signatures.
package tracing

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// contextKey is used to store the current span in a context.Context.
type contextKey struct{}

// Span represents a single tracing span.
type Span struct {
	TraceID   string
	SpanID    string
	ParentID  string
	Name      string
	StartTime time.Time
	Duration  time.Duration
	Tags      map[string]string
	ended     bool
}

// Tracer manages a stack of spans.
type Tracer struct {
	mu    sync.Mutex
	spans []*Span
	depth int // nesting depth for indentation in logs
}

// New creates a new Tracer.
func New() *Tracer {
	return &Tracer{}
}

// hexID generates a random 16-character hex string (64-bit).
func hexID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%016x", b)
}

// StartSpan creates a new child span. If the context already carries a span,
// the new span inherits its TraceID and sets the existing span as its parent.
// Returns a derived context and a function that ends the span.
func (t *Tracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
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
			return
		}
		span.ended = true
		span.Duration = time.Since(span.StartTime)

		indent := ""
		if depth > 1 {
			indent = stringsRepeat("  ", depth-1)
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

// spanFromContext retrieves the current Span from the context.
func spanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(contextKey{}).(*Span)
	return s
}

// tagsToAttrs converts a tags map to slog.Attr slice.
func tagsToAttrs(tags map[string]string) []any {
	attrs := make([]any, 0, len(tags)*2)
	for k, v := range tags {
		attrs = append(attrs, slog.String(k, v))
	}
	return attrs
}

func stringsRepeat(s string, count int) string {
	if count <= 0 {
		return ""
	}
	b := make([]byte, len(s)*count)
	for i := range count {
		copy(b[i*len(s):], s)
	}
	return string(b)
}
