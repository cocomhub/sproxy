// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
)

var (
	Version = "dev"
	BuildAt = "unknown"
)

func main() {
	if err := Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sclient:", err)
		if errors.Is(err, errInspectDiff) {
			os.Exit(3) // 审查发现差异（--disk）：便于脚本门禁，区别于执行错误
		}
		os.Exit(1)
	}
}
