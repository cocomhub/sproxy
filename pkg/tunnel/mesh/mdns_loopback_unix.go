// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

// listenMDNSLoopback 是非 Windows 平台 mDNS 测试收敛路径的组播收发端：把组播的加入
// 范围收敛到 loopback 接口（net.ListenMulticastUDP 的 ifi 参数）。
//
// 与 Windows 实现（mdns_loopback_windows.go 绑单播回环地址）不同的原因——内核投递
// 语义：Linux/BSD 只把组播投递给绑**通配地址或组地址**的 socket，绑 127.0.0.1 的
// socket 即使 JoinGroup 成功也一包收不到（实测：绑 127.0.0.1 的 0 收，绑 0.0.0.0 的
// 两个 socket 都收到）。这些平台不存在 Windows 那种「绑通配地址触发防火墙授权弹窗」
// 的问题，故沿用 ListenMulticastUDP（它自带 SO_REUSEADDR，同机多实例可绑同一端口）。
//
// loopbackInterface() 返回 nil 时 fail-closed（返回错误），不静默回落全接口绑定。
func listenMDNSLoopback(_ context.Context, group *net.UDPAddr) (*net.UDPConn, *ipv4.PacketConn, error) {
	ifi := loopbackInterface()
	if ifi == nil {
		return nil, nil, errors.New("mdns: loopback 收敛模式下未找到 loopback 接口（拒绝回落全接口绑定）")
	}
	conn, err := net.ListenMulticastUDP("udp4", ifi, group)
	if err != nil {
		return nil, nil, fmt.Errorf("mdns: 收敛到 loopback 接口加入组播 %s 失败: %w", group, err)
	}
	pc := ipv4.NewPacketConn(conn)
	// 组播回环：同机多实例互收（x/net 跨平台实现，等价于原手写 IP_MULTICAST_LOOP=1）。
	if err := pc.SetMulticastLoopback(true); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mdns: 开启组播回环失败: %w", err)
	}
	return conn, pc, nil
}
