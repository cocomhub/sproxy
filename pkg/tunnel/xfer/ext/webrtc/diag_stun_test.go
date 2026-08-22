// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// TestSetSTUNServers_NilRestoresDefault 验证传 nil 恢复默认 STUN 列表。
func TestSetSTUNServers_NilRestoresDefault(t *testing.T) {
	SetSTUNServers([]string{"stun:stun.qq.com:3478"})
	if len(stunServers) != 1 || stunServers[0] != "stun:stun.qq.com:3478" {
		t.Fatalf("设置后 stunServers 不符: %v", stunServers)
	}
	SetSTUNServers(nil)
	if len(stunServers) != len(defaultSTUNServers) || stunServers[0] != defaultSTUNServers[0] {
		t.Fatalf("nil 应恢复默认: got %v, want %v", stunServers, defaultSTUNServers)
	}
}

// TestSetSTUNServers_FiltersEmpty 验证空串被过滤（--stun "" 不应产生无效 ICE server）。
func TestSetSTUNServers_FiltersEmpty(t *testing.T) {
	SetSTUNServers([]string{"stun:a:3478", "  ", ""})
	if len(stunServers) != 1 || stunServers[0] != "stun:a:3478" {
		t.Fatalf("空串应被过滤: got %v", stunServers)
	}
	// 恢复默认，避免影响其他测试
	SetSTUNServers(nil)
}

// TestSrflxDiag_DiagnoseBranches 验证 diagnose 在各候选状态下的诊断文案。
func TestSrflxDiag_DiagnoseBranches(t *testing.T) {
	// 用独立实例避免污染全局 lastCandidateDiag
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
