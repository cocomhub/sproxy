// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// commit_wakeup_test.go 锁定评审 R14 C3：**释放侧丢唤醒**。
//
// 缺陷：`Reservation.Commit(actual < amount)` 会让可用额度增加（reserved 减少多于 committed
// 增加），但 `commitUp` 只改账本不广播 ⇒ 已入队的 `WaitReserve` 睡到超时才 507。
// `quota_writer` / `task_account` 的 `releaseUp` 直调点同理。
//
// 修复：`commitReservation`（仅 actual < amount 时）与 `releaseReserve` 统一走
// `withWaitLock`（根等待锁 + 广播）—— 与 `quota.go` 的既有硬约束一致。

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// TestCommitBelowReserveWakesWaiter 预留 100 后 commit(10) ⇒ 在队等待者必须立即获额度。
func TestCommitBelowReserveWakesWaiter(t *testing.T) {
	t.Parallel()
	pool := NewPool(100)
	scope := pool.Scope("/tenant/alice/user", 100)

	held, err := scope.TryReserve(100)
	if err != nil {
		t.Fatalf("占满失败: %v", err)
	}

	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// commit(10) 后可用额度 = 100−10 = 90 ⇒ 等 90（不多不少）。
		res, werr := scope.WaitReserve(ctx, 90, 300*time.Millisecond)
		if werr == nil {
			res.Release()
		}
		acquired <- werr
	}()

	// 让等待者先入队（避免「先 commit 后入队」走了非等待路径而假绿）：
	// 入队点由 WaitReserve 内部的 waitMu 顺序决定，这里以短轮询确认队列出现等待者。
	waitUntil(t, 20000, func() bool { return pool.WaitStats().Waiting > 0 })

	// **提交实际 10 字节**（远小于预留 100）⇒ 释放 90 字节可用额度。
	held.Commit(10)

	select {
	case werr := <-acquired:
		if werr != nil {
			t.Fatalf("commit(actual<amount) 后等待者必须被唤醒并成功获额度，got %v\n"+
				"（未修必红：commitUp 不广播 ⇒ 等到 300ms 超时后 ErrWaitTimeout）", werr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者未被唤醒（丢唤醒）")
	}
	if got := scope.Usage(); got != 10 {
		t.Fatalf("提交后 Usage=%d, want 10", got)
	}
}

// TestReleaseReserveWakesWaiter 放弃预留同样必须唤醒等待者。
func TestReleaseReserveWakesWaiter(t *testing.T) {
	t.Parallel()
	pool := NewPool(50)
	scope := pool.Scope("/tenant/bob/user", 50)

	held, err := scope.TryReserve(50)
	if err != nil {
		t.Fatalf("占满失败: %v", err)
	}

	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, werr := scope.WaitReserve(ctx, 50, 300*time.Millisecond)
		if werr == nil {
			res.Release()
		}
		acquired <- werr
	}()
	waitUntil(t, 20000, func() bool { return pool.WaitStats().Waiting > 0 })

	held.Release()

	select {
	case werr := <-acquired:
		if werr != nil {
			t.Fatalf("释放预留后等待者必须被唤醒，got %v", werr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者未被唤醒（丢唤醒）")
	}
}

// TestWaitTimeoutErrorKindPreserved R14 NEW-3：等待超时必须可与「立即不足」区分
// （HTTP 层据此给 Retry-After；否则客户端无法判断该不该重试）。
func TestWaitTimeoutErrorKindPreserved(t *testing.T) {
	t.Parallel()
	pool := NewPool(10)
	scope := pool.Scope("/tenant/carol/user", 10)
	held, err := scope.TryReserve(10)
	if err != nil {
		t.Fatalf("占满失败: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, werr := scope.WaitReserve(ctx, 10, 50*time.Millisecond)
	if werr == nil {
		t.Fatal("应等待超时")
	}
	if !errors.Is(werr, ErrWaitTimeout) {
		t.Fatalf("必须保留 ErrWaitTimeout 语义（HTTP 层要据此给 Retry-After），got %v", werr)
	}
	if !errors.Is(werr, ErrStorageFull) {
		t.Fatalf("同时必须仍是 ErrStorageFull（既有调用方契约），got %v", werr)
	}
}

// waitUntil 自旋等待条件成立（用 runtime.Gosched 让出，避免固定 sleep —— 仓库棘轮只减不增）。
func waitUntil(t *testing.T, maxIter int, cond func() bool) {
	t.Helper()
	for range maxIter {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("自旋等待超时（前置条件未成立）")
}
