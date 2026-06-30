// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"time"
)

// defaultLogger 返回一个有效的 *slog.Logger。
// 当 l 为 nil 时返回 slog.Default()，否则原样返回。
func defaultLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// parseDuration 解析 duration 字符串，失败时返回默认值。
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
