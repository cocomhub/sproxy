// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_settlement_wiring_test.go 是 P1-b（`quota.Settlement` 接线）的**前置语义锁**。
//
// 云归档预留走 `Scope.WaitReserve`：容量**暂时**不足时必须排队等待（释放后唤醒成功），
// 而不是立即 507。`quota.Settlement` 目前只提供非排队的 `Reserve`（直写 `pool.reserveUp`），
// 因此把本处换成 Settlement 会**静默丢掉排队语义**（本用例在那种迁移下变红）。
// 待 Settlement 获得排队预留能力后，接线迁移必须保持本用例绿。

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	// wiringOwnerQuota 是 owner 上限：必须大于归档预占（源 10B + 100MiB 占位），
	// 否则走「即使全部释放也放不下」的立即失败路径，测不到排队。
	wiringOwnerQuota = int64(300 << 20)
	// wiringHeldBytes 是被在途写入占住的字节：剩余 50MiB < 归档预占 ⇒ 必须排队。
	wiringHeldBytes = int64(250 << 20)
)

// TestCloudArchiveReserveQuota_WaitsForReleasedCapacity 锁定云归档预留的排队语义：
// 「容量被在途预留占住 → 入队等待 → 释放后预留成功」，而不是立即 507。
func TestCloudArchiveReserveQuota_WaitsForReleasedCapacity(t *testing.T) {
	t.Parallel()
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", wiringOwnerQuota)
	scope := env.h.quotaBucketFor("alice", "archive")
	if scope == nil {
		t.Fatal("archive 桶 Scope 未装配（用例前置不成立）")
	}
	held, err := scope.TryReserve(wiringHeldBytes)
	if err != nil {
		t.Fatalf("占住容量失败: %v", err)
	}
	held.Commit(wiringHeldBytes)

	type reserveOutcome struct {
		ok       bool
		pre      int64
		reserved int64
	}
	done := make(chan reserveOutcome, 1)
	rec := httptest.NewRecorder()
	go func() {
		res, pre, ok := env.h.cloudArchiveReserveQuota(context.Background(), rec, "alice", 10)
		out := reserveOutcome{ok: ok, pre: pre, reserved: scope.Reserved()}
		if res != nil {
			res.Release() // 本用例只锁预留（排队）语义，不进入打包/结算
		}
		done <- out
	}()

	// 确定性同步：等预留**进入根池等待队列**后再释放容量。先释放会让后到者走快路，
	// 用例就变成「未排队也绿」的假绿（无法证明排队发生）。
	waitSpin(t, 20000, func() bool { return env.h.globalPool.WaitStats().Waiting > 0 })
	scope.ReleaseUsage(wiringHeldBytes)

	select {
	case got := <-done:
		if !got.ok {
			t.Fatalf("释放容量后应预留成功（排队语义），实际 ok=false: %s", rec.Body.String())
		}
		want := int64(10) + cloudArchiveReservePlaceholder
		if got.pre != want {
			t.Fatalf("预留量 pre=%d want %d", got.pre, want)
		}
		if got.reserved != want {
			t.Fatalf("预留后 archive 桶 Reserved()=%d want %d", got.reserved, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("释放容量后预留未返回（排队未唤醒）")
	}
}
