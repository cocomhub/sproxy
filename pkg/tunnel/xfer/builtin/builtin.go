// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package builtin 确保内置传输层（TCP）已注册。
//
// xfer/internal/tcp 作为 xfer 的 internal 包，仅能被 import 路径以
// pkg/tunnel/xfer 为根的包引用；cmd/sproxy、cmd/sclient、pkg/tunnel/hub 等
// 外部调用方无法直接 blank import 它。本包是对外可见的注册桥：任何需要内置
// TCP 传输（hub 裸 TCP 中继、sclient relay --transport tcp）的包只需
//
//	import _ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
//
// 即可确保 xfer.Get("tcp") 返回已注册的内置实现。
package builtin

import (
	// 注册内置 TCP 传输层（init() 中 xfer.Register("tcp")）。
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/internal/tcp"
)
