// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"log/slog"
	"os/exec"
	"runtime"
)

// testLogger 返回静默测试日志器。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

// buildGoCmd 构造编译命令（跨平台：Windows 用 go build 同款）。
func buildGoCmd(bin, src string) *exec.Cmd {
	args := []string{"build", "-o", bin, src}
	if runtime.GOOS == "windows" {
		// go build 直接可用（Go 工具链跨平台同语义）
	}
	return exec.Command("go", args...)
}

// discardWriter 供测试与静默日志使用。
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
