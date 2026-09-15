// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import "log/slog"

// Option 是 New 的函数式装配选项。
type Option func(*tracerOptions)

// tracerOptions 是 New 的可配置项集合。
type tracerOptions struct {
	// logger 是 span 行的落地点；nil ⇒ 回退 slog.Default()。
	logger *slog.Logger
}

// WithLogger 指定 span 行落地的 logger（nil 等价于不指定：回退 slog.Default()）。
//
// 无论传入哪个 logger，其 handler 都会被 WithContextHandler 包装（已包装则跳过，
// 避免每行出现两份 trace_id/span_id），因此 span 行经带 ctx 的记录落地时自动携带
// trace_id/span_id，无需手工拼 attr、注入的 logger 也不会丢链路标识。
func WithLogger(logger *slog.Logger) Option {
	return func(o *tracerOptions) { o.logger = logger }
}
