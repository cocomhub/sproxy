// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

// restart_signal_unix.go 是 Unix 平台的优雅重启信号/继承实现。
//
// Windows 无 SIGUSR2 投递且 os/exec.ExtraFiles 不支持跨进程套接字继承 → 优雅重启
// 特性天然关闭（restart_signal_windows.go 桩），行为零变化。

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// registerRestartSignalUnix 在 Unix 上把 SIGUSR2 加入 signal.Notify（经
// runSignalHandler 的 signalChan 统一注册）。
func registerRestartSignalUnix(_ chan os.Signal) {
	// runSignalHandler 的 signal.Notify 列表已含 SIGUSR2（见 root.go 平台差异注释）。
}

// isRestartSignalUnix 判定信号是否为 SIGUSR2（Unix 优雅重启信号）。
func isRestartSignalUnix(sig os.Signal) bool {
	return sig == syscall.SIGUSR2
}

// inheritListener 返回继承的 HTTP listener：SPROXY_INHERIT_FD 存在 → 从 fd 重建
// （net.FileListener；非法 fd 报错 fail-fast，避免误接管）；否则回退 net.Listen
// （现逻辑原样，零回归）。供子进程启动路径（startPlain/TLSListener）消费。
func inheritListener(addr string) (net.Listener, error) {
	if fd, ok := inheritRestartFd(); ok {
		ln, err := net.FileListener(os.NewFile(uintptr(fd), "sproxy-inherit-http"))
		if err != nil {
			return nil, fmt.Errorf("继承监听 fd %d 失败（fail-fast，拒绝误接管）: %w", fd, err)
		}
		return ln, nil
	}
	return net.Listen("tcp", addr)
}
