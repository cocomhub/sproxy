// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"log/slog"
	"os/exec"
)

// testLogger 返回静默测试日志器。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

// buildGoCmd 构造编译命令（跨平台：Windows 用 go build 同款）。
func buildGoCmd(bin, src string) *exec.Cmd {
	args := []string{"build", "-o", bin, src}
	return exec.Command("go", args...)
}
