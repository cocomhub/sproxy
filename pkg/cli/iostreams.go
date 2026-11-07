// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pkg/cli/iostreams.go
package cli

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// IOStreams 封装 CLI 命令的输入输出流。
// 测试时可通过注入 strings.Builder 捕获输出，无需 CaptureStdout 包装。
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
	// reporter 非 nil 时，WriteErrLine 会置位（供进程入口判断「错误已由命令打印」）。
	// 设计意图：命令的 WriteErrLine 是脚本可依赖的稳定 stderr 文案（`--json` 下 stdout
	// 仍保持纯 JSON），main 仅对「命令未打印过」的错误补一行，避免重复打印。
	reporter *atomic.Bool
}

// WithErrReporter 返回设置了错误上报器的副本：WriteErrLine 会置位 b。
func (ios IOStreams) WithErrReporter(b *atomic.Bool) IOStreams {
	ios.reporter = b
	return ios
}

// SystemIOStreams 返回指向标准输入/输出/错误流的 IOStreams。
func SystemIOStreams() IOStreams {
	return IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
}

// WriteErrLine 格式化写入 ErrOut（末尾追加换行），并标记「本进程已打印过错误」。
func (ios IOStreams) WriteErrLine(format string, args ...any) {
	fmt.Fprintf(ios.ErrOut, format+"\n", args...)
	if ios.reporter != nil {
		ios.reporter.Store(true)
	}
}

// WriteWarnLine 与 WriteErrLine 同写 ErrOut，但**不**标记「已打印错误」。
// 用于「警告后命令继续运行」的场景（报文路失败回退、周期任务失败重试、事件流降级等）
// —— 若这类警告也置位，会吞掉命令随后返回的真正错误。
func (ios IOStreams) WriteWarnLine(format string, args ...any) {
	fmt.Fprintf(ios.ErrOut, format+"\n", args...)
}

// WriteOutLine 格式化写入 Out，末尾追加换行。
func (ios IOStreams) WriteOutLine(format string, args ...any) {
	fmt.Fprintf(ios.Out, format+"\n", args...)
}
