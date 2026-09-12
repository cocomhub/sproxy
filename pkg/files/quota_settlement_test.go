// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// quota_settlement_test.go 补齐两处此前无包内覆盖的**账本结算**能力：
//   - `UploadRoute.Commit`：写面双账本（owner 全局 Scope + 卷容量池）的预留结算——新文件
//     Commit(written)，覆盖写按 (prev, written) 差分 Adjust + Release；
//   - `Service.ReleaseVersionUsage`：删除版本文件后按文件大小释放 version 桶 Scope 与
//     所在卷容量池（size<=0 为空操作）。
//
// 二者都是「预留 → 结算」的收口点，跑偏会直接造成配额虚高/虚低，此前只有装配层集成测试
// 间接覆盖，域包内为零。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestUploadRoute_Commit_NewFileCommitsBothLedgers 覆盖新文件（prev=0）：两条账本都按
// 实际写入量 Commit（预留转已确认占用）。
//
// 两条账本是**彼此独立的根池**（owner 全局 Scope 挂在全局池上；卷容量池是另一个池），
// 故用两个 pool 构造，避免把「Scope 向父链聚合」误当成双账本重复记账。
func TestUploadRoute_Commit_NewFileCommitsBothLedgers(t *testing.T) {
	scope := quota.NewPool(0).Scope("/tenant/alice", 0)
	volumePool := quota.NewPool(0)

	scopeRes, err := scope.TryReserve(10)
	if err != nil {
		t.Fatalf("scope 预留: %v", err)
	}
	poolRes, err := volumePool.TryReserve(10)
	if err != nil {
		t.Fatalf("卷池预留: %v", err)
	}
	route := UploadRoute{Scope: scope, Pool: volumePool, ScopeRes: scopeRes, PoolRes: poolRes}

	route.Commit(0, 10)

	if got := scope.Usage(); got != 10 {
		t.Fatalf("owner Scope 已确认占用=%d want 10", got)
	}
	if got := volumePool.Usage(); got != 10 {
		t.Fatalf("卷池已确认占用=%d want 10", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("owner Scope 预留应结算为 0, got %d", got)
	}
	if got := volumePool.Reserved(); got != 0 {
		t.Fatalf("卷池预留应结算为 0, got %d", got)
	}
}

// TestUploadRoute_Commit_OverwriteAdjustsBothLedgers 覆盖覆盖写（prev>0）：按
// (prev, written) 差分收敛已确认占用，并释放本次预留（旧文件占用已计入 committed）。
func TestUploadRoute_Commit_OverwriteAdjustsBothLedgers(t *testing.T) {
	scope := quota.NewPool(0).Scope("/tenant/alice", 0)
	volumePool := quota.NewPool(0)

	// 模拟旧文件已确认占用 prev=8（两条账本同语义）。
	scope.Adjust(0, 8)
	volumePool.Adjust(0, 8)

	scopeRes, err := scope.TryReserve(10)
	if err != nil {
		t.Fatalf("scope 预留: %v", err)
	}
	poolRes, err := volumePool.TryReserve(10)
	if err != nil {
		t.Fatalf("卷池预留: %v", err)
	}
	route := UploadRoute{Scope: scope, Pool: volumePool, ScopeRes: scopeRes, PoolRes: poolRes}

	route.Commit(8, 10)

	if got := scope.Usage(); got != 10 {
		t.Fatalf("覆盖写后 owner Scope=%d want 10（差分收敛）", got)
	}
	if got := volumePool.Usage(); got != 10 {
		t.Fatalf("覆盖写后卷池=%d want 10（差分收敛）", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("覆盖写应释放预留, owner Scope reserved=%d", got)
	}
	if got := volumePool.Reserved(); got != 0 {
		t.Fatalf("覆盖写应释放预留, 卷池 reserved=%d", got)
	}
}

// TestUploadRoute_Commit_NoLedgersIsNoop 覆盖未装配账本（quota 未启用 / 无卷集合）：
// Commit 必须安全空操作，不得 panic——这是单卷零配额部署的常态路径。
func TestUploadRoute_Commit_NoLedgersIsNoop(t *testing.T) {
	route := UploadRoute{}
	route.Commit(0, 10)
	route.Commit(8, 10)
}

// TestService_ReleaseVersionUsage_ReleasesScope 覆盖 version 桶 Scope 释放与 size<=0 空操作。
func TestService_ReleaseVersionUsage_ReleasesScope(t *testing.T) {
	env := newDirsEnv(t)
	scope := env.quotaScopeFor("alice", "version")
	if scope == nil {
		t.Fatal("version 桶 Scope 应可用（装配了配额池）")
	}
	res, err := scope.TryReserve(5)
	if err != nil {
		t.Fatalf("预留: %v", err)
	}
	res.Commit(5)
	if got := scope.Usage(); got != 5 {
		t.Fatalf("前置占用=%d want 5", got)
	}

	env.svc.ReleaseVersionUsage(nil, "alice", 0) // size<=0：空操作
	if got := scope.Usage(); got != 5 {
		t.Fatalf("size<=0 不应释放, got %d", got)
	}

	env.svc.ReleaseVersionUsage(env.tenantFor("alice"), "alice", 5)
	if got := scope.Usage(); got != 0 {
		t.Fatalf("释放后 version 桶占用应为 0, got %d", got)
	}
}

// TestService_ReleaseVersionUsage_ReleasesVolumePool 覆盖多卷场景下版本字节同时释放
// **所在卷容量池**（T6c 双账本，与 SaveVersion 写侧对称）。
func TestService_ReleaseVersionUsage_ReleasesVolumePool(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	pool := env.pools["main"]
	if pool == nil {
		t.Fatal("默认卷容量池应已装配")
	}
	pool.Adjust(0, 7)
	tnt := env.tenantFor("alice")
	if tnt == nil {
		t.Fatal("alice 租户不可用")
	}

	env.svc.ReleaseVersionUsage(tnt, "alice", 7)
	if got := pool.Usage(); got != 0 {
		t.Fatalf("释放后所在卷容量池占用应为 0, got %d", got)
	}
}
