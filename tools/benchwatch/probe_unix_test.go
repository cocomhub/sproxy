// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// processAlive 判断 pid 是否存活（Unix：信号 0 探活；ESRCH=不存在，EPERM=存在但无权限）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

// killByPID 强制结束单个 pid（仅测试用：红跑时清理孤儿，避免污染开发机）。
func killByPID(pid int) {
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
}
