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
}
