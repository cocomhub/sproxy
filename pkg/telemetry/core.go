// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

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
	// depth 是嵌套层级（根 span = 1），由 tracer 在 StartSpan 时按父 span 递推。
	// 无导出：仅用于 slog 实现的日志缩进，不是 tracing 协议的一部分。
	depth int
}
