// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"testing"
)

func TestDialAllowed(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		// IP 直写：回环/私有/链路本地/多播/未指定 → 拒绝
		{"loopback", "127.0.0.1:22", false},
		{"loopback-v6", "[::1]:22", false},
		{"private-10", "10.0.0.5:8080", false},
		{"private-192", "192.168.1.100:22", false},
		{"private-172", "172.16.3.9:443", false},
		{"link-local", "169.254.10.20:80", false},
		{"multicast", "224.0.0.1:80", false},
		{"unspecified", "0.0.0.0:80", false},
		// 公网 IP → 允许
		{"public-ip", "8.8.8.8:53", true},
		{"public-ip-v6", "[2606:4700:4700::1111]:443", true},
		// 主机名：解析后按 IP 校验；.invalid 为 RFC 2606 保留、必然解析失败 → 拒绝
		{"hostname-unresolvable", "no-such-host.invalid:22", false},
		// 非法输入 → 拒绝
		{"bad-no-port", "127.0.0.1", false},
		{"bad-garbage", ":::", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DialAllowed(tc.addr); got != tc.want {
				t.Fatalf("DialAllowed(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestNewDialPolicy(t *testing.T) {
	// 默认（无白名单）等价 DialAllowed：私网拒绝
	def := NewDialPolicy(nil)
	if def("192.168.1.10:22") {
		t.Fatal("default policy should reject private")
	}
	if !def("8.8.8.8:53") {
		t.Fatal("default policy should allow public")
	}

	// 显式白名单：放行内网网段
	withCidr := NewDialPolicy([]string{"192.168.0.0/16", "10.0.0.0/8"})
	if !withCidr("192.168.1.10:22") {
		t.Fatal("policy with cidr should allow 192.168.1.10")
	}
	if !withCidr("10.1.2.3:8080") {
		t.Fatal("policy with cidr should allow 10.x")
	}
	// 白名单之外的内网仍拒绝
	if withCidr("172.16.0.5:80") {
		t.Fatal("policy with cidr should reject non-whitelisted private 172.x")
	}
	// 公网仍放行
	if !withCidr("8.8.8.8:53") {
		t.Fatal("policy with cidr should still allow public")
	}
	// 非法输入拒绝
	if withCidr("no-port") {
		t.Fatal("invalid addr should be rejected")
	}
}
