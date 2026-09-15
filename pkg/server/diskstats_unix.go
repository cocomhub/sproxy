// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server

import (
	"context"
	"log/slog"
	"math"
	"syscall"
)

// diskStats 返回指定目录所在磁盘的总大小、可用空间和使用量（字节）。
// ctx 用于把 trace_id/span_id 带进日志；log 为调用方的日志器（调用方负责非 nil；nil 会 panic）。
func diskStats(ctx context.Context, dir string, log *slog.Logger) (total, free, used int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		log.WarnContext(ctx, "diskStats: Statfs 失败", "dir", dir, "error", err)
		return 0, 0, 0, err
	}
	// stat.Blocks/stat.Bavail 类型因平台而异（uint64 或 int64），统一转换后 clamp 到 MaxInt64 防止溢出
	total = clampInt64(uint64(stat.Blocks) * uint64(stat.Bsize))
	free = clampInt64(uint64(stat.Bavail) * uint64(stat.Bsize))
	used = total - free
	return
}

// clampInt64 将 uint64 值限制在 MaxInt64 内，防止 int64 溢出。
func clampInt64(v uint64) int64 {
	if v > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(v)
}
