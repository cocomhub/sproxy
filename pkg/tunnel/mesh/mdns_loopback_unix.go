// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package mesh

import (
	"net"

	"golang.org/x/sys/unix"
)

// setMulticastLoopback 开启组播回环（同机多实例互收自己的组播发送）。
// Go 1.26 起 net.UDPConn 不再提供 SetMulticastLoopback，经 SyscallConn 直接
// setsockopt（x/sys/unix）。Linux 默认已开启，显式设置保证各平台一致。
func setMulticastLoopback(conn *net.UDPConn) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1)
	})
}
