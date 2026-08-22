// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"net"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// TestSetSTUNServers_NilRestoresDefault 验证传 nil 恢复默认 STUN 列表。
func TestSetSTUNServers_NilRestoresDefault(t *testing.T) {
	t.Cleanup(func() { SetSTUNServers(nil) })
	SetSTUNServers([]string{"stun:stun.qq.com:3478"})
	if len(stunServers) != 1 || stunServers[0] != "stun:stun.qq.com:3478" {
		t.Fatalf("设置后 stunServers 不符: %v", stunServers)
	}
	SetSTUNServers(nil)
	if len(stunServers) != len(defaultSTUNServers) || stunServers[0] != defaultSTUNServers[0] {
		t.Fatalf("nil 应恢复默认: got %v, want %v", stunServers, defaultSTUNServers)
	}
}

// TestSetSTUNServers_FiltersEmpty 验证空串与非法 URL 被过滤（--stun "" 不应产生无效 ICE server）。
func TestSetSTUNServers_FiltersEmpty(t *testing.T) {
	t.Cleanup(func() { SetSTUNServers(nil) })
	SetSTUNServers([]string{"stun:a:3478", "  ", ""})
	if len(stunServers) != 1 || stunServers[0] != "stun:a:3478" {
		t.Fatalf("空串应被过滤: got %v", stunServers)
	}
	// 非法 scheme 的 URL 应被过滤掉
	SetSTUNServers([]string{"stun:ok:3478", "http://bad:3478", "turn:relay:3478?transport=udp"})
	if len(stunServers) != 2 {
		t.Fatalf("非法 URL 应被过滤: got %v", stunServers)
	}
}

// TestSrflxDiag_DiagnoseBranches 验证 diagnose 在各候选状态下的诊断文案。
func TestSrflxDiag_DiagnoseBranches(t *testing.T) {
	// 用独立实例验证各分支（诊断状态已改为 per-connection，无全局累积）。
	hostOnly := &srflxDiag{}
	hostCand := &srflxDiag{}
	hostCand.record(&webrtc.ICECandidate{Typ: webrtc.ICECandidateTypeHost})
	srflxCand := &srflxDiag{}
	srflxCand.record(&webrtc.ICECandidate{Typ: webrtc.ICECandidateTypeHost})
	srflxCand.record(&webrtc.ICECandidate{Typ: webrtc.ICECandidateTypeSrflx})
	relayCand := &srflxDiag{}
	relayCand.record(&webrtc.ICECandidate{Typ: webrtc.ICECandidateTypeRelay})

	cases := []struct {
		name        string
		diag        *srflxDiag
		stunEnabled bool
		wantSubstr  string
	}{
		{"未配 STUN", hostOnly, false, "未配置 STUN"},
		{"配了 STUN 但无候选", hostOnly, true, "未收集到任何 ICE 候选"},
		{"配了 STUN 仅 host", hostCand, true, "仅 host 候选"},
		{"配了 STUN 有 srflx", srflxCand, true, "已获取公网候选"},
		{"relay 也算公网", relayCand, true, "已获取公网候选"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.diag.diagnose(tc.stunEnabled)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("diagnose(%v) = %q, 期望包含 %q", tc.stunEnabled, got, tc.wantSubstr)
			}
		})
	}
}

// TestRemoteIPFilter 验证远程 ICE 候选过滤策略（H1-S1 安全边界）。
// 默认拒 loopback/link-local/multicast/unspecified/broadcast；
// 私网（RFC1918+ULA）默认放行（保 LAN mesh），rejectPrivate 时收紧拒绝。
func TestRemoteIPFilter(t *testing.T) {
	cases := []struct {
		name   string
		ip     string // 空串表示 nil IP
		reject bool
		want   bool // true = keep
	}{
		{"loopback IPv4", "127.0.0.1", false, false},
		{"loopback IPv6", "::1", false, false},
		{"link-local unicast IPv4", "169.254.1.1", false, false},
		{"link-local unicast IPv6", "fe80::1", false, false},
		{"multicast IPv4", "224.0.0.1", false, false},
		{"multicast IPv6", "ff02::1", false, false},
		{"unspecified", "0.0.0.0", false, false},
		{"broadcast", "255.255.255.255", false, false},
		{"private IPv4 默认放行", "192.168.1.5", false, true},
		{"private IPv4 收紧拒", "192.168.1.5", true, false},
		{"ULA IPv6 默认放行", "fd00::1", false, true},
		{"ULA IPv6 收紧拒", "fd00::1", true, false},
		{"公网 IPv4", "8.8.8.8", false, true},
		{"公网 IPv6", "2606:4700:4700::1111", false, true},
		{"nil IP fail-closed", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter := remoteCandidateFilter(tc.reject)
			var ip net.IP
			if tc.ip != "" {
				ip = net.ParseIP(tc.ip)
				if ip == nil {
					t.Fatalf("ParseIP(%q) 失败", tc.ip)
				}
			}
			if got := filter(ip); got != tc.want {
				t.Fatalf("filter(%q, reject=%v) = %v, want %v", tc.ip, tc.reject, got, tc.want)
			}
		})
	}
}

// TestValidSTUNURL 验证 STUN/TURN URL 预校验（S17）。
func TestValidSTUNURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"stun:stun.l.google.com:19302", true},
		{"stuns:stun.example.com:5349", true},
		{"turn:turn.example.com:3478?transport=udp", true},
		{"turns:turn.example.com:5349", true},
		{"http://bad.example.com:3478", false},
		{"stun:missing-port", false},
		{"stun:", false},
		{"", false},
		{"stun:host:notaport", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := validSTUNURL(tc.url); got != tc.want {
				t.Fatalf("validSTUNURL(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}
