// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"time"
)

// treeController 是「终止被监视命令及其后代」的平台抽象。
//
// 生命周期：beforeStart（Start 之前，Unix 需在此设 Setpgid）→ Start →
// afterStart（Start 后立即接管；Windows 在此 AssignProcessToJobObject）→
// …（命令运行）… → terminate（停滞时收割整棵进程树）→ release（释放平台资源）。
//
// 实现：
//   - Unix（terminate_unix.go）：独立进程组，Setpgid + kill(-pgid) 一次覆盖全部后代；
//   - Windows（terminate_windows.go）：Job Object（KILL_ON_JOB_CLOSE + TerminateJobObject），
//     后代默认继承 Job；taskkill /T /F 仅作兜底。
type treeController interface {
	beforeStart(cmd *exec.Cmd)
	afterStart(cmd *exec.Cmd) (note string, err error)
	terminate(cmd *exec.Cmd, grace time.Duration) (used, note string, err error)
	release()
}

// newTreeController 由各平台的 build-tag 文件提供：
//
//	terminate_unix.go（!windows）  → processGroupTree{}
//	terminate_windows.go（windows） → jobTree{}
