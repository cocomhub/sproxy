// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
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

// TestStagingTracker_SingleWatcherPerCall 回归（m7 → W1 收敛后重写）：本用例原本用
// 「不足量释放触发多轮空唤醒」来暴露「每轮等待各 spawn 一个观察者」的 goroutine churn。
// W1 收敛后观察者 goroutine **整体消失**（waitq 以 select + timer 实现超时/取消），且
// **不足量释放不再产生空唤醒**（waitq.NotifyAll 只唤醒「额度能满足」者 = 设计 §3.1 I2 无惊群）。
// ⇒ 断言升级为更强的两条设计不变量：① watchers 恒 0；② 不足量释放**不**触发重试。
func TestStagingTracker_SingleWatcherPerCall(t *testing.T) {
	t.Parallel()
	pool := NewPool(100)
	scope := pool.Scope("", 0)
	q := NewStagingTracker(scope)
	q.waitTimeout = 3 * time.Second
	ctx := context.Background()
	if err := q.ReserveUsage(ctx, 90); err != nil { // 占 90/100
		t.Fatalf("预占: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- q.ReserveUsage(ctx, 50) }() // 90+50>100 → 进入等待
	testutil.WaitFor(t, 5*time.Second, func() bool { return q.waitRounds.Load() >= 1 },
		"等待者应进入排队")
	// 不足量释放（used 90→80，仍 80+50>100）⇒ 按 I2 不空唤醒；随后足量释放（used=50，
	// 50+50=100）⇒ 必被唤醒。负向面（「不足量释放不唤醒」）由 pkg/waitq 自身用例锁定
	// （TestQueue 系列 + Notify 命中即停），此处不引入 time.Sleep 做时序断言（R14 棘轮）。
	q.ReleaseUsage(10)
	q.ReleaseUsage(30) // used=50 → 50+50=100 恰好满足 ⇒ 必被唤醒
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("满足后应成功: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者未在额度足够后唤醒（丢唤醒）")
	}
	if got := q.watchers.Load(); got != 0 {
		t.Fatalf("W1 收敛后不应有观察者 goroutine（waitq 用 select + timer），实际 %d", got)
	}
}

// TestStagingTracker_ReleaseBeforeWait_NoLostWakeup 回归（评审 F4）：释放恰发生在
// 「TryReserve 失败 → 进入 cond.Wait」之间时，等待者必须**立即**观察到释放并成功，不得
// 空等到 waitTimeout。beforeWait 钩子把该竞态窗口确定化。
//
// 变异：去掉「持锁后 double-check 重试」（或让 ReleaseUsage 不持 cond.L 广播）→ 释放信号
// 丢失 → 等待者只能等 timer 广播 → 耗时≈waitTimeout → 红。
func TestStagingTracker_ReleaseBeforeWait_NoLostWakeup(t *testing.T) {
	t.Parallel()
	scope := NewPool(0).Scope("/staging/f4", 100)
	q := NewStagingTracker(scope)
	q.waitTimeout = 4 * time.Second
	ctx := context.Background()

	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("首次预留应成功: %v", err)
	}
	q.beforeWait = func() {
		q.beforeWait = nil // 只触发一次（窗口点）
		q.ReleaseUsage(100)
	}
	start := time.Now()
	if err := q.ReserveUsage(ctx, 100); err != nil {
		t.Fatalf("窗口内释放后应成功: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("释放后应立即成功（窗口内释放不得丢失唤醒）；实际耗时 %s（waitTimeout=%s）", elapsed, q.waitTimeout)
	}
	if got := q.waitRounds.Load(); got != 0 {
		t.Fatalf("double-check 命中时不应进入等待（waitRounds=%d）", got)
	}
}
