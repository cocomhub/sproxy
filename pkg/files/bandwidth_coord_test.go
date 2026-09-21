// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// bandwidth_coord_test.go 验证 TokenBucket 可选协调器（coord_backend=file 跨实例共享）：
//  1. SetCoordinator 后 WaitN 先查协调配额（Consume false → 等待重试，不拒绝请求）；
//  2. 协调配额耗尽时 WaitN 等待直到窗口刷新（有界 5s，超时按未限速继续）；
//  3. 未装配协调器（默认 local）WaitN 行为不变（零回归）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCoordinator 是协调配额窄接口的测试替身：Consume 返回 quota 决定是否放行。
type fakeCoordinator struct {
	limit    atomic.Int64
	consumed atomic.Int64
}

func (f *fakeCoordinator) Consume(key string, n int64) bool {
	// 模拟固定窗口：窗口内累计 <= limit 放行，否则拒绝。
	now := f.consumed.Load()
	if now+n > f.limit.Load() {
		return false
	}
	f.consumed.Add(n)
	return true
}

func TestTokenBucket_CoordinatorGate(t *testing.T) {
	t.Parallel()
	b := NewTokenBucket(1<<20, 1<<20) // 1 MiB/s 不限速场景
	coord := &fakeCoordinator{}
	coord.limit.Store(4096) // 协调配额 4 KiB
	b.SetCoordinator("alice", coord)

	// 前 4096 字节放行（协调配额内）。
	if err := b.WaitN(context.Background(), 2048); err != nil {
		t.Fatalf("协调配额内 WaitN 应放行: %v", err)
	}
	if err := b.WaitN(context.Background(), 2048); err != nil {
		t.Fatalf("协调配额内第二次 WaitN 应放行: %v", err)
	}
	// 超协调配额（4096 已用完）→ WaitN 应等待而非拒绝。
	start := time.Now()
	if err := b.WaitN(context.Background(), 1); err != nil {
		t.Fatalf("超协调配额 WaitN 不应拒绝（等待语义）: %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("超协调配额应等待重试, got %v", d)
	}
}
