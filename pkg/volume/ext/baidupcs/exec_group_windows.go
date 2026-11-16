// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package baidupcs

import "os/exec"

// hardenProcessTermination 在 Windows 上是空操作：无「进程组」原语（需 Job Object 接管），
// 有界性由 `exec` 的默认 Cancel（TerminateProcess 直接子进程）与 `Cmd.WaitDelay`
// （超时后强制关闭管道、让 Wait/Output 返回）共同保证。见 exec_bound.go。
func hardenProcessTermination(cmd *exec.Cmd) {}
