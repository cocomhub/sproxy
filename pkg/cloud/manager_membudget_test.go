// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"testing"

	"golang.org/x/sync/semaphore"
)

// TestCheckMemOverQuote 超配额判定：单文件估算 > 总预算 → true（跳过校验标记 unverified）。
func TestCheckMemOverQuote(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{checkMemSem: semaphore.NewWeighted(100), checkMemMax: 100}
	if !mgr.checkMemOverQuote(101) {
		t.Fatal("估算超配额应判 true（跳过校验）")
	}
	if mgr.checkMemOverQuote(100) {
		t.Fatal("估算等于配额不应判超（可排队执行）")
	}
	if mgr.checkMemOverQuote(0) {
		t.Fatal("估算 0 不应判超（无内存需求）")
	}
	// 配额禁用（nil sem）
	mgr2 := &CloudDownloadManager{checkMemSem: nil}
	if mgr2.checkMemOverQuote(9999) {
		t.Fatal("配额禁用时不应判超")
	}
}

// TestAcquireCheckMem 排队获取/释放：并发占用不超配额。
func TestAcquireCheckMem(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{checkMemSem: semaphore.NewWeighted(50), checkMemMax: 50}
	// 两段各 30 > 总量 50？不——单段排获取 30，第二段排 30 会阻塞等待第一段释放。
	// 验证：连续独占后 total ≤ 配额。
	release, ok := mgr.acquireCheckMem(t.Context(), 30)
	if !ok {
		t.Fatal("首次 acquire 30 应成功")
	}
	// 此时再 acquire 30 会阻塞（30+30>50）——用 try 不阻塞测试
	if mgr.checkMemSem.TryAcquire(21) {
		t.Fatal("30 已占用，21+30=51>50 应 acquire 失败")
	}
	release()
	// 释放后可再 acquire
	if !mgr.checkMemSem.TryAcquire(30) {
		t.Fatal("释放后应可 acquire 30")
	}
	mgr.checkMemSem.Release(30)
}

// TestAcquireCheckMem_CtxCancel 排队期间 ctx 取消 → ok=false（调用方跳过 Check），
// 信号量保持不被占用（无泄漏）。
func TestAcquireCheckMem_CancelReturnsNotOk(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{checkMemSem: semaphore.NewWeighted(50), checkMemMax: 50}
	// 占满 50
	rel, ok := mgr.acquireCheckMem(t.Context(), 50)
	if !ok {
		t.Fatal("首个 acquire 应成功")
	}
	// 已取消的 ctx → acquire 立即失败
	cancelCtx, cancel := context.WithCancel(t.Context())
	cancel()
	_, ok2 := mgr.acquireCheckMem(cancelCtx, 50)
	if ok2 {
		t.Fatal("ctx 已取消应 acquire 失败（ok=false）")
	}
	// 释放后信号量可再获取（无泄漏）
	rel()
	if !mgr.checkMemSem.TryAcquire(10) {
		t.Fatal("释放后应可 acquire（检查无占位泄漏）")
	}
	mgr.checkMemSem.Release(10)
}
