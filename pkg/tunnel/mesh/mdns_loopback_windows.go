// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package mesh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/windows"
)

// mdnsTestBindIP 是 Windows 收敛路径绑定的单播回环地址。
//
// 为什么不用通配地址：Windows 防火墙会对「绑定通配地址的 UDP socket」弹授权窗。
// 证据来源分别标注：
//   - 来源 A（控制者 spike，用户肉眼确认无弹窗）：绑 127.0.0.1 不弹窗；
//   - 来源 B（本实现的实测，本机 Windows 11 + x/net/ipv4）：绑 127.0.0.1 +
//     JoinGroup(loopback) 后，发往 224.0.0.251:<port> 的报文会被本机**已加入该组**
//     的多个同端口 socket 都收到（未 JoinGroup 的收不到）；组播回环由
//     SetMulticastLoopback(true) 保证——见 listenMDNSLoopback。
const mdnsTestBindIP = "127.0.0.1"

// listenMDNSLoopback 是 Windows 上 mDNS 测试收敛路径的组播收发端：绑**单播回环
// 地址**而非通配/组地址（这是不触发防火墙授权弹窗的关键），随后显式加入组播组并把
// 组播出口指向 loopback 接口。
//
// 必须显式设 SO_REUSEADDR（reuseAddrControl）：net.ListenPacket 不像
// net.ListenMulticastUDP 那样自带它，而同机多实例（用例里两个 mesh 节点）要绑同一
// 端口，第二次 bind 会直接 WSAEADDRINUSE（实测）。
//
// 非 Windows 平台不能这样绑——内核对组播的投递语义不同，见 mdns_loopback_unix.go。
func listenMDNSLoopback(ctx context.Context, group *net.UDPAddr) (*net.UDPConn, *ipv4.PacketConn, error) {
	ifi := loopbackInterface()
	if ifi == nil {
		return nil, nil, errors.New("mdns: loopback 收敛模式下未找到 loopback 接口（拒绝回落全接口绑定）")
	}
	addr := net.JoinHostPort(mdnsTestBindIP, strconv.Itoa(group.Port))
	lc := net.ListenConfig{Control: reuseAddrControl}
	bound, err := lc.ListenPacket(ctx, "udp4", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("mdns: 绑定 loopback 单播地址 %s 失败: %w", addr, err)
	}
	conn, ok := bound.(*net.UDPConn)
	if !ok {
		_ = bound.Close()
		return nil, nil, fmt.Errorf("mdns: loopback 监听返回 %T，期望 *net.UDPConn", bound)
	}
	pc := ipv4.NewPacketConn(conn)
	if err := pc.JoinGroup(ifi, group); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mdns: loopback 加入组播 %s 失败: %w", group, err)
	}
	// 组播出口必须显式指向 loopback，否则发包可能选错接口（对端与自身都收不到）。
	if err := pc.SetMulticastInterface(ifi); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mdns: 设置组播出口接口失败: %w", err)
	}
	// 组播回环：同机多实例互收（x/net 跨平台实现，等价于原手写 IP_MULTICAST_LOOP=1）。
	// 此处失败**致命**（与生产路径的告警降级不同，见 mdns.go 中该 syscall 的严重级别说明）：
	// 收敛路径存在的意义就是"同机多实例互收"，静默降级会让用例变绿而失效。
	if err := pc.SetMulticastLoopback(true); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("mdns: 开启组播回环失败: %w", err)
	}
	return conn, pc, nil
}

// reuseAddrControl 在 bind 前设置 SO_REUSEADDR（net.ListenConfig.Control 钩子），
// 允许同机多个 mesh 节点实例绑定同一 loopback 端口。必须在 bind **之前**设置：
// bind 之后再设不足以让别的实例随后绑定成功。
func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
