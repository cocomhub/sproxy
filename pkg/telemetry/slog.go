// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
)

// legacyContextKey is used to store the legacy *Span in a context.Context.
// It is kept private: newer code accesses the span via SpanContext (SpanContextKey)
// while WithTag/spanFromContext still rely on the full Span.
type legacyContextKey struct{}

// slogTracer implements Tracer with log/slog output.
//
// 嵌套层级不变量存放在每个 Span 自己身上（`Span.depth`），而不是 tracer 上的共享计数器：
// 计数器既需要「结束时归还」（曾漏掉 ⇒ 每请求缩进 +2 空格、CI 单 job 日志 26 MB），
// 在并发 span 下也不可能表达「我这个 span 的层级」。
type slogTracer struct {
	mu sync.Mutex
}

func newSlogTracer() *slogTracer {
	return &slogTracer{}
}

func (t *slogTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	traceID := TraceID()
	parentID := ""
	tags := make(map[string]string)
	depth := 1

	if parent := spanFromContext(ctx); parent != nil {
		traceID = parent.TraceID
		parentID = parent.SpanID
		depth = parent.depth + 1
		maps.Copy(tags, parent.Tags)
	}

	span := &Span{
		TraceID:   traceID,
		SpanID:    SpanID(),
		ParentID:  parentID,
		Name:      name,
		StartTime: time.Now(),
		Tags:      tags,
		depth:     depth,
	}

	newCtx := context.WithValue(ctx, SpanContextKey{}, SpanContext{TraceID: traceID, SpanID: span.SpanID})
	newCtx = context.WithValue(newCtx, legacyContextKey{}, span)

	return newCtx, func() {
		t.mu.Lock()
		if span.ended {
			t.mu.Unlock()
			slog.Warn("span already ended")
			return
		}
		span.ended = true
		span.Duration = time.Since(span.StartTime.(time.Time)) //nolint:errcheck

		// 缩进只表达嵌套层级（depth=1 的根 span 不缩进）；顺序 span 永远在同一层上。
		indent := ""
		if span.depth > 1 {
			indent = strings.Repeat("  ", span.depth-1)
		}

		attrs := slog.String("trace_id", span.TraceID)
		if len(span.Tags) > 0 {
			attrs = slog.Group("tags", tagsToAttrs(span.Tags)...)
		}

		// 解锁后再写日志：日志 I/O 可能阻塞（管道背压/落盘），不该占着共享锁串行化所有 span。
		t.mu.Unlock()
		slog.Info(fmt.Sprintf("%s[trace %s] %s %v", indent, span.TraceID, span.Name, span.Duration), attrs)
	}
}

// Inject writes the current span's W3C traceparent header into the carrier
// if the context carries a valid SpanContext.
func (t *slogTracer) Inject(ctx context.Context, carrier Carrier) {
	if sc, ok := ctx.Value(SpanContextKey{}).(SpanContext); ok && sc.TraceID != "" && sc.SpanID != "" {
		carrier.Set("traceparent", NewTraceparent(sc.TraceID, sc.SpanID))
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

func spanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(legacyContextKey{}).(*Span)
	return s
}

func tagsToAttrs(tags map[string]string) []any {
	attrs := make([]any, 0, len(tags)*2)
	for k, v := range tags {
		attrs = append(attrs, slog.String(k, v))
	}
	return attrs
}
