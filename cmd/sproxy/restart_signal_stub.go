// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// restart_signal_stub.go 是 USR2 优雅重启信号在 Unix/Windows 的平台收口。
//
// 设计：优雅重启核心（handleSignalRestart 等）只在 Unix（!windows）编译；
// 信号注册与判定按平台拆分——Windows 不注册 SIGUSR2（信号永不投递，行为零变化），
// Unix 注册 syscall.SIGUSR2。runSignalHandler 统一调用 registerRestartSignal /
// isRestartSignal，避免 root.go 出现平台分支常量。

import (
	"os"
	"runtime"
)

// registerRestartSignal 把优雅重启信号加入监听（平台差异：Windows 无 SIGUSR2）。
func registerRestartSignal(ch chan os.Signal) {
	if runtime.GOOS == "windows" {
		return // Windows：无 SIGUSR2 投递，优雅重启特性关闭（文档声明）
	}
	registerRestartSignalUnix(ch)
}

// isRestartSignal 判定信号是否为优雅重启信号（Windows 恒 false）。
func isRestartSignal(sig os.Signal) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return isRestartSignalUnix(sig)
}
