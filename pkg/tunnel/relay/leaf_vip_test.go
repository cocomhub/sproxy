// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"net/netip"
	"testing"
)

// TestNewVirtualIPDialPolicy 校验虚拟 IP 出口拨号策略（设计 §4.3）：
//  1. ServiceAddrs 精确匹配优先（真实 CGNAT 流量逃生口，I-1）；
//  2. 目标 ∈ 虚拟子网：==selfVIP 且端口 ∈ 白名单 → 改写 127.0.0.1:<port> 放行；
//     ==selfVIP 但端口不在白名单 → 拒绝（安全红线 C-1）；!=selfVIP → 拒绝；
//  3. 虚拟子网外回落 base（公网 + CIDR 白名单）。
func TestNewVirtualIPDialPolicy(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	selfVIP := netip.MustParseAddr("100.64.0.5")
	// 显式 allowPorts=[22]；serviceAddrs 的端口（2222、8080）自动加入白名单。
	policy := NewVirtualIPDialPolicy(subnet, selfVIP, []int{22}, nil,
		[]string{"127.0.0.1:2222", "100.64.0.5:8080"})

	cases := []struct {
		name string
		addr string
		want string // 期望返回串（"" 表示拒绝）
	}{
		// ==selfVIP 且端口 ∈ 显式 allowPorts → 改写本机
		{"self-vip-explicit-port", "100.64.0.5:22", "127.0.0.1:22"},
		// ==selfVIP 且端口 ∈ 宣告端口自动白名单（serviceAddrs 127.0.0.1:2222）→ 改写本机
		{"self-vip-announced-port", "100.64.0.5:2222", "127.0.0.1:2222"},
		// ServiceAddrs 精确命中（逃生口 I-1）：宣告地址在虚拟子网内 → 原样放行不改写
		{"announced-addr-in-subnet", "100.64.0.5:8080", "100.64.0.5:8080"},
		// ==selfVIP 但端口不在白名单 → 拒绝（未宣告的 18085 网关/SOCKS 不可达，C-1）
		{"self-vip-disallowed-port", "100.64.0.5:18085", ""},
		// !=selfVIP 但在虚拟子网 → 拒绝（防 SSRF/地址劫持）
		{"other-node-vip", "100.64.0.6:22", ""},
		// 虚拟子网外公网 → 回落 base 放行
		{"public", "8.8.8.8:53", "8.8.8.8:53"},
		// 虚拟子网外 loopback（未宣告）→ 拒绝
		{"loopback-not-announced", "127.0.0.1:9999", ""},
		// 畸形地址（无端口）→ 拒绝
		{"no-port", "100.64.0.5", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := policy(tc.addr)
			if tc.want == "" {
				if ok {
					t.Fatalf("policy(%q) 应拒绝, got %q", tc.addr, got)
				}
				return
			}
			if !ok {
				t.Fatalf("policy(%q) 应放行, got rejected", tc.addr)
			}
			if got != tc.want {
				t.Fatalf("policy(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestNewVirtualIPDialPolicy_AllowCIDRs 校验虚拟子网外回落 base 时 CIDR 白名单仍生效。
func TestNewVirtualIPDialPolicy_AllowCIDRs(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	selfVIP := netip.MustParseAddr("100.64.0.5")
	policy := NewVirtualIPDialPolicy(subnet, selfVIP, nil, []string{"10.0.0.0/8"}, nil)

	if _, ok := policy("10.1.2.3:80"); !ok {
		t.Fatal("CIDR 白名单网段应放行（回落 base）")
	}
	if _, ok := policy("172.16.0.5:80"); ok {
		t.Fatal("白名单之外私网应拒绝")
	}
	// selfVIP 端口不在白名单仍拒绝（虚拟子网分支优先于 CIDR 判断）。
	if _, ok := policy("100.64.0.5:9999"); ok {
		t.Fatal("selfVIP 非白名单端口应拒绝")
	}
}

// TestNewVirtualIPDialPolicy_NoSelfVIP 校验 selfVIP 无效（客户端未拿到虚拟 IP）时
// 虚拟子网内目标全部拒绝（fail-closed），虚拟子网外回落 base 不变。
func TestNewVirtualIPDialPolicy_NoSelfVIP(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	policy := NewVirtualIPDialPolicy(subnet, netip.Addr{}, []int{22}, nil, nil)

	if _, ok := policy("100.64.0.5:22"); ok {
		t.Fatal("selfVIP 无效时虚拟子网内目标应拒绝（fail-closed）")
	}
	if _, ok := policy("8.8.8.8:53"); !ok {
		t.Fatal("selfVIP 无效不影响虚拟子网外公网放行")
	}
}

// TestNewVirtualIPDialPolicy_RejectsIPv6Subnet 校验 IPv6 子网被拒绝（虚拟 IP 仅支持 IPv4）。
func TestNewVirtualIPDialPolicy_RejectsIPv6Subnet(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("IPv6 子网应 panic（虚拟 IP 分配仅支持 IPv4）")
		}
	}()
	NewVirtualIPDialPolicy(netip.MustParsePrefix("fd00::/8"), netip.Addr{}, nil, nil, nil)
}
