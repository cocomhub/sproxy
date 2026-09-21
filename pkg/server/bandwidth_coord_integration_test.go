// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// bandwidth_coord_integration_test.go 验证带宽限速装配层的协调后端接线：
//  1. coord_backend=file 时 bwBucketFor 返回的桶装配协调器（SetCoordinator 后
//     WaitN 经协调配额放行——跨实例共享生效）；
//  2. 默认（local）零回归：桶无协调器，WaitN 纯内存 token 桶。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestBandwidthCoord_FileBackendAssemblesCoordinator 验证 coord_backend=file 时
// bwBucketFor 的桶装配协调器：两个 Handlers 实例共用同一 storage 目录（跨实例共享），
// WaitN 总量不超过 per_owner_bps。
func TestBandwidthCoord_FileBackendAssemblesCoordinator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.RateLimit.Bandwidth.Enabled = true
	cfg.RateLimit.Bandwidth.PerOwnerBPS = 2000
	cfg.RateLimit.Bandwidth.CoordBackend = "file"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := &Handlers{cfgPtr: &cfgPtr}
	b1 := h.bwBucketFor("alice", 2000, 0)
	if b1 == nil {
		t.Fatal("file backend bucket should be created")
	}
	// 实例 2（同一 storage 目录，独立 Handlers → 独立协调器单例但共享文件计数）。
	h2 := &Handlers{cfgPtr: &cfgPtr}
	b2 := h2.bwBucketFor("alice", 2000, 0)
	if b2 == nil {
		t.Fatal("instance2 bucket should be created")
	}

	// 跨实例共享：b1 消耗 1500B 后 b2 只能消耗 ≤500B（总量 2000 共享）。
	if err := b1.WaitN(context.Background(), 1500); err != nil {
		t.Fatalf("b1 1500B WaitN: %v", err)
	}
	if err := b2.WaitN(context.Background(), 500); err != nil {
		t.Fatalf("b2 500B WaitN (total 2000): %v", err)
	}
	// 超共享预算：b2 再 1B → WaitN 等待（不拒绝），最终因协调配额不足等待到超时按未限速继续。
	start := time.Now()
	_ = b2.WaitN(context.Background(), 1) // 不拒绝，等待或超时兜底
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("超共享预算应等待（不拒绝）, got %v", d)
	}
}

// TestBandwidthCoord_LocalDefaultNoCoordinator 默认（coord_backend 空/local）零回归：
// 桶无协调器（WaitN 纯内存 token 桶行为不变）。
func TestBandwidthCoord_LocalDefaultNoCoordinator(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.RateLimit.Bandwidth.Enabled = true
	cfg.RateLimit.Bandwidth.PerOwnerBPS = 1000
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := &Handlers{cfgPtr: &cfgPtr}
	b := h.bwBucketFor("alice", 1000, 0)
	if b == nil {
		t.Fatal("local backend bucket should be created")
	}
	if err := b.WaitN(context.Background(), 100); err != nil {
		t.Fatalf("local WaitN: %v", err)
	}
}
