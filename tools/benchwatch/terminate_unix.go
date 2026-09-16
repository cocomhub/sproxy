// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// processGroupTree 用**独立进程组**终止整棵树（Unix 的既有路径，本片未改动）：
// 被监视命令成为组长，`kill(-pgid)` 一次覆盖它派生的所有后代。`go test` 会再派生 `<pkg>.test`，
// 只杀 `go` 会留下孤儿进程——CI 日志里 `Terminate orphan process: pid (…) (server.test)` 即此形态。
type processGroupTree struct{}

// beforeStart 让子进程成为独立进程组的组长（必须在 Start 之前设置）。
func (processGroupTree) beforeStart(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// afterStart 无需接管（进程组在 Start 时即建立，无竞态窗口）。
func (processGroupTree) afterStart(*exec.Cmd) (string, error) { return "", nil }

// terminate 先对整个进程组发 SIGTERM、宽限 grace 后再 SIGKILL。
func (processGroupTree) terminate(cmd *exec.Cmd, grace time.Duration) (used, note string, err error) {
	if cmd.Process == nil {
		return "", "", nil
	}
	pgid := cmd.Process.Pid
	if killErr := syscall.Kill(-pgid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return "", "", killErr
	}
	if grace > 0 {
		time.Sleep(grace)
	}
	if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return "", "", killErr
	}
	return "独立进程组（SIGTERM→宽限→SIGKILL，覆盖全部后代）", "", nil
}

// release 无平台资源需释放。
func (processGroupTree) release() {}

// newTreeController 返回 Unix 的进程组控制器。
func newTreeController() treeController { return processGroupTree{} }
