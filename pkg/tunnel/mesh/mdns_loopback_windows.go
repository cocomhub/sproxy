// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package mesh

import (
	"net"
	"syscall"
)

// ipMulticastLoop 是 Windows winsock 的 IP_MULTICAST_LOOP 选项常量
// （定义于 ws2ipdef.h，值 11；syscall 包未导出）。
const ipMulticastLoop = 11

// setMulticastLoopback 开启组播回环（同机多实例互收自己的组播发送）。
// Windows 默认关闭 IP_MULTICAST_LOOP，Go 1.26 起 net.UDPConn 不再提供
// SetMulticastLoopback，须经 SyscallConn 直接 setsockopt。
func setMulticastLoopback(conn *net.UDPConn) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, ipMulticastLoop, 1)
	})
}
