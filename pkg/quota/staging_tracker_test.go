// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestStagingTracker_ReserveRelease 基本记账：预留 + 释放后可再次预留。
func TestStagingTracker_ReserveRelease(t *testing.T) {
	t.Parallel()
	scope := NewPool(0).Scope("/staging/t", 100)
	q := NewStagingTracker(scope)
	ctx := context.Background()

	if err := q.ReserveUsage(ctx, 60); err != nil {
		t.Fatalf("预留 60 应成功: %v", err)
	}
	if got := scope.Usage(); got != 60 {
		t.Fatalf("Usage = %d, want 60", got)
	}
	q.ReleaseUsage(60)
	if got := scope.Usage(); got != 0 {
		t.Fatalf("释放后 Usage = %d, want 0", got)
	}
	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("释放后预留 100 应成功: %v", err)
	}
}

// TestStagingTracker_Timeout 永久不足 → 等待后快速失败（有界超时）。
func TestStagingTracker_Timeout(t *testing.T) {
	t.Parallel()
	scope := NewPool(0).Scope("/staging/t", 100)
	q := NewStagingTracker(scope)
	q.waitTimeout = 20 * time.Millisecond
	ctx := context.Background()

	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("首次预留应成功: %v", err)
	}
	if err := q.ReserveUsage(ctx, 100); err == nil {
		t.Fatal("永久不足应超时报错（不无限挂起）")
	}
}

// TestStagingTracker_WaitsForRelease 排队等待：并发释放后成功（不误报超时）。
func TestStagingTracker_WaitsForRelease(t *testing.T) {
	t.Parallel()
	scope := NewPool(0).Scope("/staging/t", 100)
	q := NewStagingTracker(scope)
	q.waitTimeout = 2 * time.Second
	ctx := context.Background()

	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("首次预留应成功: %v", err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		q.ReleaseUsage(100)
	}()
	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("释放后应成功（不得误报超时）: %v", err)
	}
}

// TestStagingTracker_CtxCancel ctx 取消 → 中断等待并返回可识别错误。
func TestStagingTracker_CtxCancel(t *testing.T) {
	t.Parallel()
	scope := NewPool(0).Scope("/staging/t", 100)
	q := NewStagingTracker(scope)
	q.waitTimeout = 5 * time.Second

	if err := q.ReserveUsage(context.Background(), 100); err != nil {
		t.Fatalf("首次预留应成功: %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := q.ReserveUsage(cctx, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled（包装），got %v", err)
	}
}
