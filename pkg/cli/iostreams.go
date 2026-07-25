// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// pkg/cli/iostreams.go
package cli

import (
	"fmt"
	"io"
	"os"
)

// IOStreams 封装 CLI 命令的输入输出流。
// 测试时可通过注入 strings.Builder 捕获输出，无需 CaptureStdout 包装。
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

// SystemIOStreams 返回指向标准输入/输出/错误流的 IOStreams。
func SystemIOStreams() IOStreams {
	return IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
}

// WriteErrLine 格式化写入 ErrOut，末尾追加换行。
func (ios IOStreams) WriteErrLine(format string, args ...any) {
	fmt.Fprintf(ios.ErrOut, format+"\n", args...)
}

// WriteOutLine 格式化写入 Out，末尾追加换行。
func (ios IOStreams) WriteOutLine(format string, args ...any) {
	fmt.Fprintf(ios.Out, format+"\n", args...)
}
