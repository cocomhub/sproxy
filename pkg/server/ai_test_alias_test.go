// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_test_alias_test.go 是 AI 测试的迁移期别名（实现已下沉 pkg/server/ai）。

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/server/ai"
)

// aiTask 是测试侧别名（实现已下沉 pkg/server/ai）。
type aiTask = ai.Task

func newInsightCache(dir string, logger *slog.Logger) *ai.InsightCache {
	return ai.NewInsightCache(dir, logger)
}
