// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
)

// osExitErr 打印错误到 stderr 并以非零码退出（包级间接层：测试可注入验证
// 退出路径，main 保持最薄）。
func osExitErr(err error) {
	fmt.Fprintln(os.Stderr, "sproxy-mcp:", err)
	os.Exit(1)
}
