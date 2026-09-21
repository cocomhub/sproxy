// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"testing"
	"time"
)

// TestTokenBucket_WaitNBurst 桶满时 WaitN 立即返回（不阻塞），burst 内不限速。
func TestTokenBucket_WaitNBurst(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1000, 4096) // 1KB/s, burst 4KB
	ctx := context.Background()
	start := time.Now()
	if err := b.WaitN(ctx, 1024); err != nil {
		t.Fatalf("WaitN(1024) burst 内应成功: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("burst 内 WaitN 不应阻塞, took %v", d)
	}
}

// TestTokenBucket_WaitNRefills 耗尽后 WaitN 按速率 refill（1KB/s 下 512B 需 ~512ms）。
func TestTokenBucket_WaitNRefills(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1000, 1024) // 1KB/s, burst 1KB（满桶即 1KB）
	ctx := context.Background()
	if err := b.WaitN(ctx, 1024); err != nil { // 消耗满桶
		t.Fatal(err)
	}
	start := time.Now()
	if err := b.WaitN(ctx, 512); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("耗尽后 WaitN(512) 应等待 refill ~512ms, got %v", d)
	}
}

// TestTokenBucket_ContextCancel WaitN 在 ctx 取消时返回错误。
func TestTokenBucket_ContextCancel(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1, 1) // 极慢：1B/s burst 1B
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.WaitN(ctx, 1024); err == nil {
		t.Fatal("ctx 取消后 WaitN 应返回错误")
	}
}

// TestTokenBucket_ZeroRateDisabled rate<=0 表示不限速（默认关零回归）。
func TestTokenBucket_ZeroRateDisabled(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(0, 0)
	start := time.Now()
	if err := b.WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("rate=0 不应限速: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("rate=0 WaitN 不应阻塞, took %v", d)
	}
}

// TestLimiterPerOwner 每个 owner 独立桶：一 owner 限速不影响另一 owner（per-owner 隔离）。
func TestLimiterPerOwner(t *testing.T) {
	t.Parallel()
	lm := NewBandwidthLimiter(func(owner string) *TokenBucket {
		if owner == "slow" {
			return NewTokenBucket(100, 100) // 100B/s burst 100B
		}
		return nil // 其它 owner 不限速
	})
	if lm.BucketFor("slow") == nil {
		t.Fatal("slow owner 应有桶")
	}
	if lm.BucketFor("fast") != nil {
		t.Fatal("fast owner 不应有桶（不限速）")
	}
	// slow 桶消耗后 fast 不受影响。
	slow := lm.BucketFor("slow")
	if err := slow.WaitN(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if err := lm.BucketFor("fast").WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("fast owner WaitN 应不受 slow 影响: %v", err)
	}
}
