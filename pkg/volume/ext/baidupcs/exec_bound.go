// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"os/exec"
	"time"
)

// exec_bound.go 装配「外部二进制调用的有界执行」（超时 + 强制收束 + 进程树终止）。
//
// **为什么需要**（生产实测 + 本机 Linux 复现）：`exec.CommandContext` 的超时只对**直接子进程**
// 发 kill；而 `Cmd.Output`/`Cmd.Wait` 还要等 stdout/stderr 管道关闭（I/O 拷贝 goroutine，
// 见 Go issue #23019）。CLI 挂死时若它派生并持有管道的后代仍存活，`Wait` 会一直等 ⇒
// 配置的超时**无法让调用返回**（= 实际无限；现象：启动期 `BaiduPCS-Go` 挂死 ⇒ 启动永久阻塞、
// 端口不监听）。Go 为此提供 `Cmd.WaitDelay`：从「ctx 取消」起再限一段时间收管道。

// binaryExecGrace 是超时取消后的「收束窗口」：到点强制关闭管道并让 Wait/Output 返回
// （`exec.ErrWaitDelay`）。取值只需覆盖「管道里残留数据的排空」，故 2s 足够且远小于任何业务超时。
const binaryExecGrace = 2 * time.Second

// newBoundedCmd 构造带「有限超时 + 强制收束」的子命令：
//   - ctx/timeout 到点 ⇒ 取消子进程（平台侧见 hardenProcessTermination）；
//   - 取消后最多再等 binaryExecGrace 收管道 ⇒ **保证 Output/Wait 有界返回**；
//   - cancel 由调用方 defer（释放定时器，避免 ctx 泄漏）。
func newBoundedCmd(ctx context.Context, timeout time.Duration, bin string, args ...string) (*exec.Cmd, context.CancelFunc) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	cmd := exec.CommandContext(cmdCtx, bin, args...)
	cmd.WaitDelay = binaryExecGrace
	hardenProcessTermination(cmd)
	return cmd, cancel
}
