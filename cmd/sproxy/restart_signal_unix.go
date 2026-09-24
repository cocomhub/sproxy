// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// registerRestartSignalUnix 在 Unix 上把 SIGUSR2 加入 signal.Notify（经
// runSignalHandler 的 signalChan 统一注册）。
func registerRestartSignalUnix(ch chan os.Signal) {
	// SIGUSR2 优雅重启信号（roadmap 11.6-②）。
	_ = ch // signal.Notify 在 runSignalHandler 内统一调用（含重启信号）
	// 无需额外操作：runSignalHandler 的 signal.Notify 列表已含 SIGUSR2
	// （见 root.go registerRestartSignal 平台差异注释）。
}

// isRestartSignalUnix 判定信号是否为 SIGUSR2（Unix 优雅重启信号）。
func isRestartSignalUnix(sig os.Signal) bool {
	return sig == syscall.SIGUSR2
}
