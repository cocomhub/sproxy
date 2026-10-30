// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ip.go 是 IP/CIDR 列表的通用解析与判定：配置校验（config 包）与运行时匹配
// （pkg/server 的 IP 门/可信代理解析）共用同一份实现，避免两处各写一遍解析规则。
package netutil

import "net"

// ParseIPNets 把 IP/CIDR 列表解析为 *net.IPNet 列表。
//
// 语义：CIDR 原样解析；纯 IP 按 /32（IPv4）或 /128（IPv6）归一；
// 非法条目跳过（配置校验阶段已 fail-closed 拒绝，此处不重复报错）。
func ParseIPNets(list []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		if _, ipnet, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, ipnet)
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return nets
}

// AllowIPsCoverLoopback 判定 allow_ips 是否同时覆盖 IPv4 与 IPv6 回环。
//
// 用途：启动告警辅助——回环也须显式在白名单（无自动放行双通道），配漏
// 127.0.0.1/::1 时由装配层打印告警，防止运维误把自己锁在门外。
func AllowIPsCoverLoopback(allowIPs []string) bool {
	hasV4, hasV6 := false, false
	for _, n := range ParseIPNets(allowIPs) {
		if n.Contains(net.ParseIP("127.0.0.1")) {
			hasV4 = true
		}
		if n.Contains(net.ParseIP("::1")) {
			hasV6 = true
		}
	}
	return hasV4 && hasV6
}
