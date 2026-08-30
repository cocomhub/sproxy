// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// NewVirtualIPDialPolicy 构造带虚拟 IP NAT 语义的出口拨号策略（设计 §4.3）。
//
// 命中顺序（I-1 逃生口）：
//  1. 先对 ServiceAddrs 精确字符串匹配放行（与 NewServiceDialPolicy 一致）——
//     宣告在虚拟子网内的真实服务地址仍可访问，不被虚拟子网判断遮蔽；
//  2. 目标 host ∈ 虚拟子网：==selfVIP 且端口 ∈ 白名单 → 改写 127.0.0.1:<port>
//     放行（虚拟主机语义：拨到本机 <port> 服务）；==selfVIP 但端口不在白名单
//     → 拒绝（C-1：未宣告的网关 18085/SOCKS/agent socket 不可被 mesh 触达）；
//     !=selfVIP → 拒绝（虚拟 IP 属于其他节点，防 SSRF/地址劫持）；
//  3. 虚拟子网外回落 NewDialPolicy(allowCIDRs) 的既有公网 + CIDR 白名单逻辑。
//
// 端口白名单 = 显式 allowPorts（--vip-allow-port）∪ serviceAddrs 各条目端口
// （--service 宣告端口自动开放）。selfVIP 无效（客户端未拿到虚拟 IP）时虚拟子网
// 内目标全部拒绝（fail-closed），虚拟子网外行为不变。
//
// 仅支持 IPv4 虚拟子网（配置路径 config.Validate 已拦截 IPv6）；传 IPv6 前缀会
// panic（编程错误，立即暴露）。
func NewVirtualIPDialPolicy(subnet netip.Prefix, selfVIP netip.Addr, allowPorts []int, allowCIDRs, serviceAddrs []string) func(string) (string, bool) {
	if !subnet.Addr().Is4() {
		panic(fmt.Sprintf("relay: NewVirtualIPDialPolicy 需要 IPv4 虚拟子网，got %s", subnet))
	}
	base := NewDialPolicy(allowCIDRs)
	exact := make(map[string]struct{}, len(serviceAddrs))
	allowSet := make(map[int]struct{}, len(allowPorts)+len(serviceAddrs))
	for _, a := range serviceAddrs {
		exact[a] = struct{}{}
	}
	for _, p := range allowPorts {
		if p > 0 && p <= 65535 {
			allowSet[p] = struct{}{}
		}
	}
	// 宣告端口自动加入白名单（--service 端口即出口本机开放端口）。
	for _, a := range serviceAddrs {
		if _, port, err := net.SplitHostPort(a); err == nil {
			if p, perr := strconv.Atoi(port); perr == nil && p > 0 && p <= 65535 {
				allowSet[p] = struct{}{}
			}
		}
	}
	return func(addr string) (string, bool) {
		// 1. 宣告地址精确匹配优先（逃生口）。
		if _, ok := exact[addr]; ok {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || port == "" {
				return "", false
			}
			if ip := net.ParseIP(host); ip != nil {
				return net.JoinHostPort(ip.String(), port), true
			}
			ips, lerr := net.LookupIP(host)
			if lerr != nil || len(ips) == 0 {
				return "", false
			}
			return net.JoinHostPort(ips[0].String(), port), true
		}
		// 2. 虚拟子网分支（虚拟主机语义 + 端口白名单）。
		host, port, err := net.SplitHostPort(addr)
		if err != nil || port == "" {
			return "", false
		}
		ip, perr := netip.ParseAddr(host)
		if perr == nil && subnet.Contains(ip) {
			if ip != selfVIP {
				return "", false // 虚拟 IP 属于其他节点 → 拒绝（SSRF/地址劫持防护）。
			}
			p, atoiErr := strconv.Atoi(port)
			if atoiErr != nil || p <= 0 || p > 65535 {
				return "", false
			}
			if _, allowed := allowSet[p]; !allowed {
				return "", false // 端口不在白名单 → 拒绝（C-1）。
			}
			return "127.0.0.1:" + port, true // 改写为本机服务。
		}
		// 3. 虚拟子网外回落既有公网 + CIDR 白名单逻辑。
		return base(addr)
	}
}
