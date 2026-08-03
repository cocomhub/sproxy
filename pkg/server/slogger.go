// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
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

// parseDuration 解析 duration 字符串，失败时返回错误。
func parseDuration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}
