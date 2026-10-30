// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package alerts

import (
	"log/slog"
	"testing"
)

// testLogger 返回丢弃输出的日志器（随测试迁入本包）。
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

var _ = testing.Short
