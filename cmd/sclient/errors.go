// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

const (
	errFmtInitClient      = "初始化客户端失败: %w"
	errFmtInitClientPrint = "初始化客户端失败: %v"
	errFmtInvalidPath     = "无效的路径: %w"
	errFmtMkdirFailed     = "创建目录失败: %w"

	// schemeWSS 是 wss:// 前缀（hub 地址 scheme 归一/校验共享）。
	schemeWSS = "wss://"

	// msgBatchSkipped 是批处理取消时的结果文案。
	msgBatchSkipped = "Skipped（任务已取消）"
	// errFmtWatchSyncFail 是 watch 同步失败日志格式。
	errFmtWatchSyncFail = "watch: 同步任务失败: %v"
)
