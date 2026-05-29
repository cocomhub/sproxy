// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
)

// defaultLogger 返回一个有效的 *slog.Logger。
// 当 l 为 nil 时返回 slog.Default()，否则原样返回。
func defaultLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
