// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

// exitCodeFor 把错误映射为进程退出码：审查发现差异 = 3（脚本门禁判据），其余 = 1。
func exitCodeFor(err error) int {
	if errors.Is(err, errInspectDiff) {
		return 3
	}
	return 1
}

// reportError 是错误输出的**单点收口**（独立函数便于单测）：
//   - 命令内 WriteErrLine 打印的是脚本可依赖的稳定 stderr 文案（`--json` 下 stdout 仍为纯
//     JSON），故保留不删；这里仅在「命令未打印过错误」（reported=false）时补一行
//     `sclient: <err>`，消除重复打印。
//   - `--json`：额外把错误以结构化对象写 stdout（`{"success":false,"error":...,"exit_code":N}`），
//     让脚本无需解析 stderr 文案。审查差异（errInspectDiff）已由命令输出结构化结果，不重复输出。
func reportError(err error, jsonMode, reported bool, stdout, stderr io.Writer) {
	code := exitCodeFor(err)
	if jsonMode && !errors.Is(err, errInspectDiff) {
		_ = json.NewEncoder(stdout).Encode(map[string]any{
			"success":   false,
			"error":     err.Error(),
			"exit_code": code,
		})
	}
	if !reported {
		fmt.Fprintf(stderr, "sclient: %v\n", err)
	}
}

func main() {
	err := Execute()
	if err == nil {
		return
	}
	reportError(err, cliJSONMode.Load(), cliErrReported.Load(), os.Stdout, os.Stderr)
	os.Exit(exitCodeFor(err))
}
