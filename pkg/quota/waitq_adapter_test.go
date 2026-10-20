// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// waitq_adapter_test.go 锁定 W1 收敛（设计 docs/designs/2026-10-19-waitq.md §8）后的
// **能力等价**要点 —— 迁移到 pkg/waitq 时最容易丢的就是这些边界语义。

import (
	"context"
	"testing"
	"time"
)

// TestWaitReserve_ZeroBytesPassthrough 零字节**直通**（能力不丢失）：waitq 要求 N>0，
// 而空文件/mkdir 等路径会传 0 —— 既有语义是「立即成功、不排队」，必须原样保留
// （回归来源：迁移后曾把 0 判为非法 ⇒ 远程写整条链路 500）。
func TestWaitReserve_ZeroBytesPassthrough(t *testing.T) {
	t.Parallel()
	p := NewPool(1024)
	before := p.WaitRounds()

	res, err := p.WaitReserve(context.Background(), 0, time.Second)
	if err != nil {
		t.Fatalf("零字节应直通成功（既有语义）, got %v", err)
	}
	if res == nil {
		t.Fatal("零字节应返回非 nil 预留（调用方按其 Commit/Release 语义使用）")
	}
	res.Release()
	if got := p.WaitRounds(); got != before {
		t.Fatalf("零字节不得进入等待队列（waitRounds 不应变化）: before=%d after=%d", before, got)
	}
	if got := p.Usage(); got != 0 {
		t.Fatalf("零字节预留不应改变账本, committed=%d", got)
	}
}

// TestWaitReserve_NegativeBytesKeepsTryReserveSemantics 负值仍由 TryReserve 决定语义
// （不 panic、不被 waitq 的 N>0 校验改写）。
func TestWaitReserve_NegativeBytesKeepsTryReserveSemantics(t *testing.T) {
	t.Parallel()
	p := NewPool(1024)
	_, wantErr := p.TryReserve(-1)
	_, gotErr := p.WaitReserve(context.Background(), -1, time.Second)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("负值语义应与 TryReserve 一致: TryReserve=%v WaitReserve=%v", wantErr, gotErr)
	}
}

// TestWaitReserve_AdapterTakesRealReservation 等待成功后的预留是**真实**的：
// 账本已计入（committed/reserved），且 Commit/Release 语义与 TryReserve 一致
// （waitq 的 Lease 不承载配额语义 ⇒ 适配器必须把 Reservation 交给调用方，且不得误释放）。
func TestWaitReserve_AdapterTakesRealReservation(t *testing.T) {
	t.Parallel()
	p := NewPool(100)
	res, err := p.WaitReserve(context.Background(), 60, time.Second)
	if err != nil {
		t.Fatalf("额度充足应成功: %v", err)
	}
	// Reservation 未 Commit ⇒ 计入 Reserved（而非 Usage/committed）。
	if got := p.Reserved(); got != 60 {
		t.Fatalf("预留后 reserved 应为 60, got %d", got)
	}
	res.Release()
	if got := p.Reserved(); got != 0 {
		t.Fatalf("释放后 reserved 应为 0, got %d", got)
	}
	if got := p.Usage(); got != 0 {
		t.Fatalf("释放后 committed 应为 0, got %d", got)
	}
}

// TestWaitReserve_TooLargeFailsFast 不可能容纳 ⇒ **立即失败不排队**（严格上界判据保留：
// 迁移前由 canNeverFit 保证，现由 waitq 的 ErrTooLarge 判据 + 适配器 Capacity 承担）。
func TestWaitReserve_TooLargeFailsFast(t *testing.T) {
	t.Parallel()
	p := NewPool(100)
	before := p.WaitRounds()
	start := time.Now()
	_, err := p.WaitReserve(context.Background(), 101, 30*time.Second)
	if err == nil {
		t.Fatal("超过上限应失败")
	}
	if got := p.WaitRounds(); got != before {
		t.Fatalf("不可能容纳不得排队（waitRounds 不应变化）: before=%d after=%d", before, got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("应立即失败（不得白等超时）, took %v", d)
	}
}
