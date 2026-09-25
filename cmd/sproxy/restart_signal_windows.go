// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

// restart_signal_windows.go 是 Windows 平台的优雅重启信号/继承桩。
//
// Windows 无 SIGUSR2 投递且 os/exec.ExtraFiles 不支持跨进程套接字继承 → 优雅重启
// 特性天然关闭（registerRestartSignal 空操作、isRestartSignal 恒 false、inheritListener
// 恒普通 net.Listen），行为零变化。handleSignalRestart 是符号兼容桩：正常流程不会
// 走到（isRestartSignal 恒 false）。

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/server"
)

// registerRestartSignalUnix 是 Windows 桩（供 stub 的统一签名调用；空操作）。
func registerRestartSignalUnix(_ chan os.Signal) {}

// isRestartSignalUnix 恒 false（Windows 无优雅重启信号）。
func isRestartSignalUnix(_ os.Signal) bool { return false }

// inheritListener Windows 版：恒普通 net.Listen（无 fd 继承，零回归）。
func inheritListener(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// handleSignalRestart Windows 桩：优雅重启不适用（不会被调用）。
func handleSignalRestart(_ context.CancelFunc, _ *http.Server, _ *server.Handlers, _ *slog.Logger, _ *server.Config) {
}
