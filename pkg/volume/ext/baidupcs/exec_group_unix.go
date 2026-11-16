// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package baidupcs

import (
	"os"
	"os/exec"
	"syscall"
)

// hardenProcessTermination 把子进程放进**独立进程组**，并把超时终止改为对整组发 SIGKILL。
//
// `exec.CommandContext` 默认的 Cancel 只 kill 直接子进程；CLI 派生的后代仍存活并持有
// stdout/stderr 管道 ⇒ `Wait` 继续阻塞（超时形同虚设），且每次超时都留一批孤儿进程。
// 独立进程组（`Setpgid` 在 fork+exec 之间由子进程自身调用，失败即 Start 失败 ⇒ 组 id 恒等于
// 子进程 pid）保证 `-pgid` 只覆盖该子进程树，绝不会波及 sproxy 自身。
func hardenProcessTermination(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// 进程组已不存在（ESRCH）等 ⇒ 退回杀直接子进程，保持 exec 默认语义。
			return cmd.Process.Kill()
		}
		return nil
	}
}
