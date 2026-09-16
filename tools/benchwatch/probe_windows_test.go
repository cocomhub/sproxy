// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

// processAlive 在 Windows 上借用 `tasklist` 探活：标准库没有等价原语
// （`os.FindProcess` 恒成功，`Process.Signal` 仅支持 Kill）。
//
// 判据是 CSV 输出里出现 `,"<pid>",`（避免把镜像名里的数字误当 pid）。
// `tasklist` 不可用时**保守返回 true**（宁可让用例超时失败，也不误报「已终止」）。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return true
	}
	return strings.Contains(string(out), `,"`+strconv.Itoa(pid)+`",`)
}

// killByPID 强制结束单个 pid（仅测试用：红跑时清理孤儿，避免污染开发机）。
func killByPID(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}
