// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// alerts_quota_test.go 验证配额预警（roadmap P2 配额预警）：
//  1. quota_watermark source：per-owner 水位超阈值 → 告警（FIRING 去抖）。
//  2. 恢复（<阈值）→ 恢复通知。
//  3. 多 owner 各自独立（owner-a 85% 告警不影响 owner-b 未超）。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAlertEngine_QuotaWatermark per-owner 水位 85% 触发 + 恢复。
func TestAlertEngine_QuotaWatermark(t *testing.T) {
	t.Parallel()
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules: []AlertRule{{
			Source: "quota_watermark", Threshold: 80, Channels: []string{"wecom"},
		}},
	}, nil)
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetQuotaUsageReader(func(owner string) (used, cap int64) {
		if owner == "alice" {
			return 850, 1000 // 85% > 80%
		}
		return 500, 1000 // 50% < 80%
	})

	// alice 85% → 告警。
	eng.checkQuotaWatermarks(context.Background(), []string{"alice", "bob"})
	select {
	case m := <-ch.msgs:
		if !strings.Contains(m, "alice") {
			t.Fatalf("告警应含 owner: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("alice 85% 应触发配额告警")
	}
	// 同水位再触发 → 去抖。
	eng.checkQuotaWatermarks(context.Background(), []string{"alice", "bob"})
	select {
	case <-ch.msgs:
		t.Fatal("同水位二次触发应去抖")
	case <-time.After(100 * time.Millisecond):
	}
	// alice 恢复 50% → 恢复通知。
	eng.SetQuotaUsageReader(func(owner string) (used, cap int64) {
		return 500, 1000
	})
	eng.checkQuotaWatermarks(context.Background(), []string{"alice", "bob"})
	select {
	case <-ch.msgs:
	default:
		t.Fatal("alice 恢复应发恢复通知")
	}
}
