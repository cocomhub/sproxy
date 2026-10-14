// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// wait_reserve_test.go 钉住配额模型 v3 的两项保障（2026-10-14 用户裁定）：
//   - **运维余量**：配额（用量 + 分配）只能用 `容量 − 余量` ⇒ 非配额物理字节（staging/孤儿/索引）
//     有固定空间 ⇒「绝对禁止磁盘打满」由预留保证；
//   - **排队重试**：严格上界预占不足时不立即拒绝，而是排队等待（释放广播 / ctx / 30s 超时）
//     ⇒「真正放得下的文件最终一定成功」。
//
// 变异：去掉 usable 判断 ⇒ 运维余量用例红；广播改到非根节点 / 去掉 double-check ⇒ 跨层唤醒或唤醒用例红。

// waitForRoundsOrDone 同 waitForRounds，但若等待者已提前返回则直接报出其错误（诊断用）。
func waitForRoundsOrDone(t *testing.T, p *Pool, rounds int64, done chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.WaitRounds() >= rounds {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("等待者提前返回（应排队等待）: %v", err)
		default:
		}
		runtime.Gosched()
	}
	t.Fatalf("等待者未进入等待（waitRounds=%d want >=%d）", p.WaitRounds(), rounds)
}

// TestOperationalReserve_LimitsQuotaUse：运维余量收紧用量与分配两条路径。
func TestOperationalReserve_LimitsQuotaUse(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)
	p.SetOperationalReserve(100) // 运维余量 100 ⇒ 配额可用 900
	if got := p.Usable(); got != 900 {
		t.Fatalf("Usable = %d want 900", got)
	}
	if _, err := p.TryReserve(950); err == nil {
		t.Fatal("用量超可用上限（950 > 900）必须被拒")
	}
	res, err := p.TryReserve(900)
	if err != nil {
		t.Fatalf("恰好用满可用上限应通过: %v", err)
	}
	res.Commit(900)
	// **分配账本与用量是两个独立账本**（配额模型 v3 明确）：用量满不阻止「把额度分配出去」——
	// 分配是策略划分（Σ子上限 ≤ 父可用上限），物理安全由**用量检查沿父链**保证（子节点的预留会
	// 撞上父节点已满的用量 ⇒ 失败）。故此处分配 100 应成功。
	if _, err := p.TryAllocate(100); err != nil {
		t.Fatalf("用量满不阻止分配额度（独立账本）: %v", err)
	}
	// 但「分配 + 用量」都不能突破可用上限：已分配 100 ⇒ 再分配 850 应失败（100+850 > 900）。
	if _, err := p.TryAllocate(850); err == nil {
		t.Fatal("分配总额超可用上限（100+850 > 900）必须被拒")
	}
	// 释放用量后，分配空间按可用上限计算（与用量无关）。
	p.ReleaseCommitted(900)
	if _, err := p.TryAllocate(950); err == nil {
		t.Fatal("分配超可用上限（950 > 900）必须被拒")
	}
	if _, err := p.TryAllocate(800); err != nil {
		t.Fatalf("分配 900 应通过: %v", err)
	}
	if got := p.Unallocated(); got != 0 {
		t.Fatalf("Unallocated = %d want 0", got)
	}
	if got := p.OperationalReserve(); got != 100 {
		t.Fatalf("OperationalReserve = %d want 100", got)
	}
}

// TestWaitReserve_WakesOnRelease：不足时排队，释放后唤醒并成功（不误拒）。
func TestWaitReserve_WakesOnRelease(t *testing.T) {
	t.Parallel()
	p := NewPool(100)
	full, err := p.TryReserve(100)
	if err != nil {
		t.Fatalf("预置占满: %v", err)
	}
	full.Commit(100)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, werr := p.WaitReserve(ctx, 50, 5*time.Second)
		if werr == nil {
			res.Commit(50)
		}
		done <- werr
	}()
	// **确定性同步**：轮询 waitRounds 直到等待者确实进入等待（不依赖 time.Sleep）。
	waitForRoundsOrDone(t, p, 1, done)
	p.ReleaseCommitted(100) // 释放 ⇒ 广播唤醒
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("释放后应预留成功: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后未被唤醒（丢唤醒）")
	}
	if got := p.Usage(); got != 50 {
		t.Fatalf("唤醒后用量 = %d want 50", got)
	}
}

// TestWaitReserve_TimeoutReturnsStorageFull：无人释放 ⇒ 超时返回包装 ErrStorageFull（调用方映射 507）。
func TestWaitReserve_TimeoutReturnsStorageFull(t *testing.T) {
	t.Parallel()
	p := NewPool(100)
	full, _ := p.TryReserve(100)
	full.Commit(100)
	start := time.Now()
	_, err := p.WaitReserve(context.Background(), 50, 150*time.Millisecond)
	if err == nil {
		t.Fatal("超时应失败")
	}
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("错误应包装 ErrStorageFull（容量不足语义）: %v", err)
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("错误应可归因「等待超时」: %v", err)
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("应在超时窗口后才失败，实际 %s", el)
	}
}

// TestWaitReserve_CtxCancel：ctx 取消 ⇒ 立即返回 ctx 错误（不占满超时窗口）。
func TestWaitReserve_CtxCancel(t *testing.T) {
	t.Parallel()
	p := NewPool(100)
	full, _ := p.TryReserve(100)
	full.Commit(100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.WaitReserve(ctx, 50, 10*time.Second)
		done <- err
	}()
	waitForRoundsOrDone(t, p, 1, done) // 确定性同步：等待者已进入等待
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回 ctx 取消错误, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消未打断等待")
	}
}

// TestWaitReserve_CrossLevelWake：等待者挂在**子节点**、容量在**父节点**被释放 ⇒ 仍被唤醒
// （验证「等待统一挂根节点队列」的设计：跨层释放不得丢唤醒）。
func TestWaitReserve_CrossLevelWake(t *testing.T) {
	t.Parallel()
	vol := NewPool(100)                // 卷：容量 100
	owner := vol.Scope("alice", 100)   // 用户配额 100（= 卷上限）
	other := vol.Scope("bob", 0)       // 另一用户（无上限，只受卷约束）
	full, err := other.TryReserve(100) // bob 先占满**卷**
	if err != nil {
		t.Fatalf("bob 占满卷: %v", err)
	}
	full.Commit(100)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// alice 自己的配额有空间（100），但**卷**已满 ⇒ 失败点在父节点。
		res, werr := owner.WaitReserve(ctx, 60, 5*time.Second)
		if werr == nil {
			res.Commit(60)
		}
		done <- werr
	}()
	// 等待者挂在 alice（子节点），但失败点在**卷**（父节点）⇒ 轮询**根节点**的 waitRounds。
	waitForRoundsOrDone(t, vol, 1, done)
	other.ReleaseUsage(100) // 父节点释放 ⇒ 必须唤醒挂在子节点的等待者
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("父节点释放后应预留成功: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("父节点释放未唤醒子节点等待者（丢唤醒）")
	}
}
