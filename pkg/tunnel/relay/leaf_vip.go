// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"fmt"
	"log/slog"
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
// 端口白名单 = 显式 allowPorts（--vip-allow-port）∪ serviceAddrs 中**本机/loopback
// 宣告**条目的端口（S-2 收紧：--service 端口仅当 host 为 loopback/本机 IP 时才是
// 本机开放端口）。远程 LAN 宣告（--service db:192.168.1.10:5432）的端口**不**自动
// 进入白名单——避免 mesh connect <selfVIP>:5432 意外暴露本机同端口未宣告服务；
// 需开放远程宣告端口时用 --vip-allow-port 显式加入。
//
// 注意：
//   - 在**自身虚拟 IP 上**宣告服务（--service x:100.64.0.5:8080）会精确匹配优先
//     返回原地址不改写（I-1 逃生口语义），实际拨号到 VIP IP（通常不可达）而非
//     本机——如需开放本机 8080 应用 --vip-allow-port 8080 并在 serviceAddrs 用
//     loopback 地址宣告；
//   - 虚拟子网内**未宣告**的真实 CGNAT 地址（==selfVIP 但端口不在白名单 / !=selfVIP）
//     一律拒绝（fail-closed 设计意图，防 SSRF/地址劫持），比旧策略的"全部 CGNAT
//     放行"更窄，I-1 逃生口只保已宣告地址。
//
// selfVIP 无效（客户端未拿到虚拟 IP）时虚拟子网内目标全部拒绝（fail-closed），
// 虚拟子网外行为不变。仅支持 IPv4 虚拟子网（配置路径 config.Validate 已拦截
// IPv6）；传 IPv6 前缀会 panic（编程错误，立即暴露）。
func NewVirtualIPDialPolicy(subnet netip.Prefix, selfVIP netip.Addr, allowPorts []int, allowCIDRs, serviceAddrs []string) func(string) (string, bool) {
	if !subnet.Addr().Is4() {
		panic(fmt.Sprintf("relay: NewVirtualIPDialPolicy 需要 IPv4 虚拟子网，got %s", subnet))
	}
	// F-2：selfVIP 有效但不在策略子网内 → 虚拟子网分支永不命中，对自身虚拟 IP 的
	// 拨号将 fail-closed 拒绝（功能静默失效）。提示运维核对 hub.virtual_subnet 与
	// 客户端虚拟子网配置（mesh node --virtual-subnet）是否一致。
	if selfVIP.IsValid() && !subnet.Contains(selfVIP) {
		slog.Warn("虚拟 IP 不在出口拨号策略子网内，虚拟 IP 拨号将 fail-closed 拒绝（请检查 hub.virtual_subnet 与客户端虚拟子网配置是否一致）", "self_vip", selfVIP, "subnet", subnet)
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
	// 宣告端口自动加入白名单（S-2 收紧：**仅当服务 host 为 loopback/本机 IP** 时，
	// 其端口才是"本机开放端口"。远程 LAN 宣告（如 --service db:192.168.1.10:5432）
	// 的端口**不**进入白名单——否则 mesh connect <selfVIP>:5432 会改写拨到本机
	// 127.0.0.1:5432，意外暴露本机同端口未宣告服务。需要开放远程宣告端口时用
	// --vip-allow-port 显式加入）。
	for _, a := range serviceAddrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			continue
		}
		if !isLocalHost(host) {
			continue
		}
		if p, perr := strconv.Atoi(port); perr == nil && p > 0 && p <= 65535 {
			allowSet[p] = struct{}{}
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

// isLocalHost 判断 host 是否为 loopback 或本机网卡 IP（S-2：仅本机服务的端口进入
// 虚拟 IP 白名单）。主机名仅认 "localhost"（解析为本机 loopback）；其他主机名（含
// 本机主机名、远程 host 名）保守判为远程（不自动开放端口，可 --vip-allow-port 显式
// 加入）——避免依赖 DNS 解析导致误判。
func isLocalHost(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
			return true
		}
	}
	return false
}
