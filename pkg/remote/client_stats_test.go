// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote_test

// client_stats_test.go 验证 federated 卷本地配额统计（roadmap P2 联邦卷回写残余）：
//  1. B 端装配 owner 配额卷 → A 经隧道 Stats 取到配额（Used/MaxBytes）。
//  2. B 端无配额卷 → Stats 返回 Quota=nil（不失败）。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestClient_Stats_QuotaViaTunnel B 端配额卷 → A Stats 取到配额。
func TestClient_Stats_QuotaViaTunnel(t *testing.T) {
	t.Parallel()
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// B 端装配 owner 配额。
	b := startBEndQuota(t, aID.Fingerprint(), 1<<30, 0) // 1GiB 上限，初始用量 0
	c := newAClient(t, b, aID)
	st, err := c.Stats(t.Context(), remote.Ref{Node: testNodeA, Volume: testVol})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st == nil || st.Quota == nil {
		t.Fatalf("配额卷应返回 Quota，got nil")
	}
	if st.Quota.MaxBytes != 1<<30 {
		t.Fatalf("MaxBytes = %d want %d", st.Quota.MaxBytes, 1<<30)
	}
	if st.Quota.Usage < 0 {
		t.Fatalf("Usage < 0: %d", st.Quota.Usage)
	}
}

// TestClient_Stats_NoQuota_B_Nil B 端无配额卷 → Quota=nil（不失败）。
func TestClient_Stats_NoQuota_B_Nil(t *testing.T) {
	t.Parallel()
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b := startBEnd(t, aID.Fingerprint())
	c := newAClient(t, b, aID)
	st, err := c.Stats(t.Context(), remote.Ref{Node: testNodeA, Volume: testVol})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st == nil {
		t.Fatalf("Stats 应返回响应（quota 段可能为空）")
	}
	if st.Quota != nil {
		t.Fatalf("无配额卷 Quota 应为 nil，got %+v", st.Quota)
	}
}

// startBEndQuota 同 startBEnd 但装配 per-owner 配额（配额卷）：
// quotaBytes = owner 配额上限（0=不限）；usedBytes 为初始预置用量（写一个占位文件）。
func startBEndQuota(t *testing.T, aFP string, quotaBytes int64, usedBytes int64) *bEnd {
	t.Helper()
	b := startBEnd(t, aFP)
	// 装配 owner 配额（重启 config——直接改 b.cfg 生效于 /api/stats quota 段）。
	b.cfg.OwnerQuotas = map[string]server.ByteSize{testOwner: server.ByteSize(quotaBytes)}
	if usedBytes > 0 {
		writeBFile(t, b.cfg, "quota-probe.txt", make([]byte, usedBytes))
	}
	return b
}
