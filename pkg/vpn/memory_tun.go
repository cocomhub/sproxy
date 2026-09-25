// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vpn

import (
	"io"
)

// NewMemoryTUN 返回一个内存 tun 设备（P1 装配兜底）：Read 立即 EOF（无真实
// 应用流量），Write 丢弃。用于 mesh up --tun 在平台实现 Open fail-closed
// （P1 不做真设备）时装配 Router，保证 CLI 形态与 P2 真设备无缝衔接。
// 生产路径不会被误用：P2 平台实现 Open 返回真设备句柄，不再走本兜底。
func NewMemoryTUN() io.ReadWriteCloser {
	return emptyConn{}
}
