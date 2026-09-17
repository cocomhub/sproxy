// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"testing"
)

// TestQuota_ReserveRelease 预留 → 释放 → 用量归零。
func TestQuota_ReserveRelease(t *testing.T) {
	t.Parallel()
	q := NewQuota(1024)
	if err := q.Reserve(400); err != nil {
		t.Fatalf("Reserve(400): %v", err)
	}
	if err := q.Reserve(600); err != nil {
		t.Fatalf("Reserve(600): %v", err)
	}
	if got := q.Used(); got != 1000 {
		t.Fatalf("Used = %d, want 1000", got)
	}
	q.Release(400)
	q.Release(600)
	if got := q.Used(); got != 0 {
		t.Fatalf("Used after release = %d, want 0", got)
	}
}

// TestQuota_ExceedLimit 超限返回错误（拒绝继续写 staging）。
func TestQuota_ExceedLimit(t *testing.T) {
	t.Parallel()
	q := NewQuota(100)
	if err := q.Reserve(100); err != nil {
		t.Fatalf("Reserve(100): %v", err)
	}
	if err := q.Reserve(1); err == nil {
		t.Fatal("超限 Reserve 应报错")
	}
}

// TestQuota_Unlimited 上限 0 = 不限。
func TestQuota_Unlimited(t *testing.T) {
	t.Parallel()
	q := NewQuota(0)
	for i := 0; i < 1000; i++ {
		if err := q.Reserve(1024); err != nil {
			t.Fatalf("不限上限应全部允许: %v", err)
		}
	}
}

// TestQuota_CacheCap cache 独立上限：超 cap 拒绝。
func TestQuota_CacheCap(t *testing.T) {
	t.Parallel()
	cacheQuota := NewQuota(500)
	if err := cacheQuota.Reserve(500); err != nil {
		t.Fatalf("Reserve(500): %v", err)
	}
	if err := cacheQuota.Reserve(1); err == nil {
		t.Fatal("cache 超 cap 应拒绝")
	}
}

// TestQuota_ReleaseOverflow 释放超过已用量不 panic、归零。
func TestQuota_ReleaseOverflow(t *testing.T) {
	t.Parallel()
	q := NewQuota(1024)
	_ = q.Reserve(10)
	q.Release(9999)
	if got := q.Used(); got != 0 {
		t.Fatalf("Used = %d, want 0", got)
	}
}
