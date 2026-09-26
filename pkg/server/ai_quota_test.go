// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"log/slog"
	"testing"
)

// TestAIQuota_CheckAndCharge 验证限 3 次 → 第 4 次 ErrQuotaExceeded。
func TestAIQuota_CheckAndCharge(t *testing.T) {
	t.Parallel()
	q := NewAIQuota(AIQuotaConfig{
		Enabled: true,
		Default: AILimit{DailyCalls: 3},
	}, slog.Default())
	for i := range 3 {
		if err := q.CheckAndCharge("ownerA", 100); err != nil {
			t.Fatalf("第 %d 次应通过: %v", i+1, err)
		}
	}
	if err := q.CheckAndCharge("ownerA", 100); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("第 4 次应 ErrQuotaExceeded, got %v", err)
	}
}

// TestAIQuota_DailyReset 验证 Day 变更 → 清零。
func TestAIQuota_DailyReset(t *testing.T) {
	t.Parallel()
	q := NewAIQuota(AIQuotaConfig{
		Enabled: true,
		Default: AILimit{DailyCalls: 2},
	}, slog.Default())
	_ = q.CheckAndCharge("ownerA", 1)
	q.setDay("2000-01-02") // 注入不同日
	if err := q.CheckAndCharge("ownerA", 1); err != nil {
		t.Fatalf("跨天应清零: %v", err)
	}
	u := q.Usage("ownerA")
	if u.Calls != 1 {
		t.Fatalf("calls = %d, want 1（跨天重置后计数）", u.Calls)
	}
}

// TestAIQuota_DisabledNoop 验证 Enabled=false → 恒通过零回归。
func TestAIQuota_DisabledNoop(t *testing.T) {
	t.Parallel()
	q := NewAIQuota(AIQuotaConfig{Enabled: false}, slog.Default())
	for range 100 {
		if err := q.CheckAndCharge("ownerA", 1); err != nil {
			t.Fatalf("禁用应恒通过: %v", err)
		}
	}
}

// TestAIQuota_Overrides 验证 per-owner 覆盖生效。
func TestAIQuota_Overrides(t *testing.T) {
	t.Parallel()
	q := NewAIQuota(AIQuotaConfig{
		Enabled:   true,
		Default:   AILimit{DailyCalls: 1},
		Overrides: map[string]AILimit{"ownerB": {DailyCalls: 5}},
	}, slog.Default())
	if err := q.CheckAndCharge("ownerA", 1); err != nil {
		t.Fatalf("ownerA 默认限 1 应通过: %v", err)
	}
	if err := q.CheckAndCharge("ownerA", 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("ownerA 第 2 次应超限: %v", err)
	}
	for i := range 5 {
		if err := q.CheckAndCharge("ownerB", 1); err != nil {
			t.Fatalf("ownerB 覆盖限 5 第 %d 次应通过: %v", i+1, err)
		}
	}
	if err := q.CheckAndCharge("ownerB", 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("ownerB 第 6 次应超限: %v", err)
	}
}

// TestAIQuota_Tokens 验证 token 估算超限。
func TestAIQuota_Tokens(t *testing.T) {
	t.Parallel()
	q := NewAIQuota(AIQuotaConfig{
		Enabled: true,
		Default: AILimit{DailyTokens: 500},
	}, slog.Default())
	if err := q.CheckAndCharge("ownerA", 400); err != nil {
		t.Fatalf("400 < 500 应通过: %v", err)
	}
	if err := q.CheckAndCharge("ownerA", 200); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("累计 600 > 500 应超限: %v", err)
	}
}
