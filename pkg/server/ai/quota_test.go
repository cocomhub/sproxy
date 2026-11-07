// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// quota_test.go 验证 per-owner AI 调用配额（AIQuota）：
//  1. enabled=false → nil（调用点判空恒通过，零回归）；
//  2. 日调用/日 token 累加与超额拒绝（超额不扣减）；
//  3. overrides 优先于 default；0 = 不限；
//  4. 跨天惰性重置（时钟注入）；SetDay 标记过期；
//  5. 并发扣减不超发（-race 下验证）。

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
)

// fixedQuota 构造带注入时钟的配额器（跨天重置用）。
func fixedQuota(t *testing.T, cfg config.AIQuotaConfig, now time.Time) *AIQuota {
	t.Helper()
	q := NewAIQuota(cfg, testAILogger())
	if q == nil {
		t.Fatal("enabled=true 应构造配额器")
	}
	q.now = func() time.Time { return now }
	return q
}

// TestAIQuota_Disabled enabled=false → nil，且 nil 接收者所有查询为无操作。
func TestAIQuota_Disabled(t *testing.T) {
	t.Parallel()
	if q := NewAIQuota(config.AIQuotaConfig{}, testAILogger()); q != nil {
		t.Fatal("enabled=false 应返回 nil（零回归）")
	}
	var q *AIQuota
	if err := q.CheckAndCharge("alice", 10); err != nil {
		t.Fatalf("nil 配额应恒通过: %v", err)
	}
	if u := q.Usage("alice"); u.Calls != 0 || u.Tokens != 0 || u.Day != "" {
		t.Fatalf("nil 配额用量应为零值: %+v", u)
	}
}

// TestAIQuota_DailyCallsLimit 日调用上限：超额返回 ErrQuotaExceeded 且不扣减。
func TestAIQuota_DailyCallsLimit(t *testing.T) {
	t.Parallel()
	q := fixedQuota(t, config.AIQuotaConfig{
		Enabled: true,
		Default: config.AILimit{DailyCalls: 2},
	}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local))

	for i := range 2 {
		if err := q.CheckAndCharge("alice", 0); err != nil {
			t.Fatalf("第 %d 次应通过: %v", i+1, err)
		}
	}
	err := q.CheckAndCharge("alice", 0)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("第 3 次应超限，got %v", err)
	}
	if u := q.Usage("alice"); u.Calls != 2 || u.Limit.DailyCalls != 2 {
		t.Fatalf("超额不应扣减，且应回报 limit: %+v", u)
	}
}

// TestAIQuota_DailyTokensLimit 日 token 估算上限（含边界相等通过）。
func TestAIQuota_DailyTokensLimit(t *testing.T) {
	t.Parallel()
	q := fixedQuota(t, config.AIQuotaConfig{
		Enabled: true,
		Default: config.AILimit{DailyTokens: 100},
	}, time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local))

	if err := q.CheckAndCharge("alice", 60); err != nil {
		t.Fatalf("60/100 应通过: %v", err)
	}
	if err := q.CheckAndCharge("alice", 40); err != nil {
		t.Fatalf("累计 100/100 应通过: %v", err)
	}
	if err := q.CheckAndCharge("alice", 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("101/100 应超限，got %v", err)
	}
	if u := q.Usage("alice"); u.Tokens != 100 || u.Calls != 2 {
		t.Fatalf("超额不应扣减: %+v", u)
	}
}

// TestAIQuota_ZeroLimitsUnlimited 两项上限都为 0 → 不限。
func TestAIQuota_ZeroLimitsUnlimited(t *testing.T) {
	t.Parallel()
	q := fixedQuota(t, config.AIQuotaConfig{Enabled: true}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))
	for range 50 {
		if err := q.CheckAndCharge("alice", 1000); err != nil {
			t.Fatalf("0 = 不限，不应超限: %v", err)
		}
	}
	if u := q.Usage("alice"); u.Calls != 50 || u.Tokens != 50000 {
		t.Fatalf("用量累计不符: %+v", u)
	}
}

// TestAIQuota_Overrides overrides 优先于 default，且按 owner 隔离。
func TestAIQuota_Overrides(t *testing.T) {
	t.Parallel()
	q := fixedQuota(t, config.AIQuotaConfig{
		Enabled:   true,
		Default:   config.AILimit{DailyCalls: 1},
		Overrides: map[string]config.AILimit{"alice": {DailyCalls: 3}},
	}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))

	for i := range 3 {
		if err := q.CheckAndCharge("alice", 0); err != nil {
			t.Fatalf("alice 第 %d 次应通过（override=3）: %v", i+1, err)
		}
	}
	if err := q.CheckAndCharge("alice", 0); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("alice 第 4 次应超限，got %v", err)
	}
	if err := q.CheckAndCharge("bob", 0); err != nil {
		t.Fatalf("bob 第 1 次应通过（default=1）: %v", err)
	}
	if err := q.CheckAndCharge("bob", 0); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("bob 第 2 次应超限，got %v", err)
	}
	if u := q.Usage("alice"); u.Limit.DailyCalls != 3 {
		t.Fatalf("Usage 应回报 override limit: %+v", u)
	}
}

// TestAIQuota_DailyReset 跨天惰性重置：次日用量清零重计。
func TestAIQuota_DailyReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local)
	q := NewAIQuota(config.AIQuotaConfig{
		Enabled: true,
		Default: config.AILimit{DailyCalls: 1, DailyTokens: 10},
	}, testAILogger())
	if q == nil {
		t.Fatal("enabled=true 应构造配额器")
	}
	q.now = func() time.Time { return now }

	if err := q.CheckAndCharge("alice", 10); err != nil {
		t.Fatalf("首日应通过: %v", err)
	}
	if u := q.Usage("alice"); u.Day != "2026-01-01" || u.Calls != 1 || u.Tokens != 10 {
		t.Fatalf("首日用量不符: %+v", u)
	}

	// 次日：用量视图清零，扣减重新从 1 开始（跨天重置）。
	now = time.Date(2026, 1, 2, 1, 0, 0, 0, time.Local)
	if u := q.Usage("alice"); u.Day != "2026-01-02" || u.Calls != 0 || u.Tokens != 0 {
		t.Fatalf("跨天用量应清零: %+v", u)
	}
	if err := q.CheckAndCharge("alice", 10); err != nil {
		t.Fatalf("次日应重新通过: %v", err)
	}
	if u := q.Usage("alice"); u.Calls != 1 || u.Tokens != 10 {
		t.Fatalf("次日用量不符: %+v", u)
	}
}

// TestAIQuota_SetDay SetDay 标记过期 → 下次 Usage/CheckAndCharge 重置。
func TestAIQuota_SetDay(t *testing.T) {
	t.Parallel()
	q := fixedQuota(t, config.AIQuotaConfig{
		Enabled: true,
		Default: config.AILimit{DailyCalls: 1},
	}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))

	if err := q.CheckAndCharge("alice", 0); err != nil {
		t.Fatalf("首日应通过: %v", err)
	}
	q.SetDay("2000-01-01")
	if u := q.Usage("alice"); u.Calls != 0 || u.Day != "2026-01-01" {
		t.Fatalf("SetDay 后 Usage 应重置: %+v", u)
	}
	if err := q.CheckAndCharge("alice", 0); err != nil {
		t.Fatalf("重置后应重新通过: %v", err)
	}
}

// TestAIQuota_ConcurrentCharging 并发扣减：成功数恰好等于上限（不超发）。
func TestAIQuota_ConcurrentCharging(t *testing.T) {
	t.Parallel()
	const (
		limit    = 100
		attempts = 200
	)
	q := fixedQuota(t, config.AIQuotaConfig{
		Enabled: true,
		Default: config.AILimit{DailyCalls: limit},
	}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))

	var okCount, exceeded atomic.Int64
	var wg sync.WaitGroup
	wg.Add(attempts)
	for range attempts {
		go func() {
			defer wg.Done()
			if err := q.CheckAndCharge("alice", 1); err != nil {
				if errors.Is(err, ErrQuotaExceeded) {
					exceeded.Add(1)
					return
				}
				t.Errorf("意外错误: %v", err)
				return
			}
			okCount.Add(1)
		}()
	}
	wg.Wait()

	if got := okCount.Load(); got != limit {
		t.Fatalf("成功次数应为 %d，got %d", limit, got)
	}
	if got := exceeded.Load(); got != attempts-limit {
		t.Fatalf("超额次数应为 %d，got %d", attempts-limit, got)
	}
	if u := q.Usage("alice"); u.Calls != limit || u.Tokens != limit {
		t.Fatalf("并发后用量不符: %+v", u)
	}
}
