// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_settlement_wiring_test.go 是 P1-b（云归档配额结算迁移到 `quota.Settlement`）的**语义锁**。
//
// 云归档预留走 `Settlement.ReserveWait`：容量**暂时**不足时必须排队等待（释放后唤醒成功），
// 而不是立即 507——换成非排队的 `Settlement.Reserve`（直写 `pool.reserveUp`）会**静默丢掉排队
// 语义**；排队上限必须取 `h.quotaWaitTimeout`（0 = 生产默认 30s）——改回硬编码
// `quota.DefaultWaitTimeout` 会**静默让测试注入的等待上限失效**。两个用例各自锁定其中一条。

import (
	"context"
	"net/http"
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

// TestCloudArchiveReserveQuota_HonorsInjectedWaitTimeout 锁定「归档预留的排队上限取
// h.quotaWaitTimeout」（#963 记录的真实缺陷：此处此前恒传 quota.DefaultWaitTimeout，测试夹具
// 注入的等待上限被静默忽略；生产恒 0 ⇒ 仍按 30s 默认，故生产行为不变）。
//
// 判据选「排队超时返回 507 且耗时远小于默认 30s」，而不是断言精确 50ms：时限断言只用于区分
// 「注入生效」与「硬编码 30s」，不引入时序 flaky。
func TestCloudArchiveReserveQuota_HonorsInjectedWaitTimeout(t *testing.T) {
	t.Parallel()
	env := newOwnerEnv(t)
	env.setOwnerQuota("alice", wiringOwnerQuota)
	scope := env.h.quotaBucketFor("alice", "archive")
	if scope == nil {
		t.Fatal("archive 桶 Scope 未装配（用例前置不成立）")
	}
	// 占住容量且**不释放**：预占（源 10B + 100MiB 占位）放不下 ⇒ 入队等待到超时。
	held, err := scope.TryReserve(wiringHeldBytes)
	if err != nil {
		t.Fatalf("占住容量失败: %v", err)
	}
	held.Commit(wiringHeldBytes)

	// 夹具前提：注入值必须远小于默认 30s，否则本用例无法区分「注入生效」与「硬编码」。
	if got := env.h.quotaWaitTimeout; got <= 0 || got >= 5*time.Second {
		t.Fatalf("夹具前提：quotaWaitTimeout=%v 应为小值（测试夹具注入异常）", got)
	}

	rec := httptest.NewRecorder()
	start := time.Now()
	res, pre, ok := env.h.cloudArchiveReserveQuota(context.Background(), rec, "alice", 10)
	elapsed := time.Since(start)
	if ok {
		if res != nil {
			res.Release()
		}
		t.Fatalf("容量被占住且无人释放，预留必须超时失败（ok=true pre=%d）", pre)
	}
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("排队超时必须回 507，got %d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("排队耗时 %v ⇒ 注入的 h.quotaWaitTimeout 未生效（硬编码 quota.DefaultWaitTimeout 回归）", elapsed)
	}
	// 超时失败必须全额回滚：账本无残留预留，且等待者已出队。
	waitSpin(t, 20000, func() bool { return env.h.globalPool.WaitStats().Waiting == 0 })
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("超时后 archive 桶 Reserved()=%d want 0（预留泄漏）", got)
	}
}
