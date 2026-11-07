// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build tools

// tools 聚合仓库用到的构建工具依赖（tools.go 模式：go.mod/go.sum 锁定版本，CI 经
// go run 解析走 go.sum 校验——S8545 锁文件强制）。
package tools

import _ "github.com/google/addlicense"
