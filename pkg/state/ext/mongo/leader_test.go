// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mongo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/state"
)

// TestMongoLeaderElector_TryAcquire 核心租约语义：A 获取 → B 失败（非错误）→
// A 释放 → B 成功。
func TestMongoLeaderElector_TryAcquire(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "lease")
	// 两个独立实例（模拟两个节点）。
	a, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#a: %v", err)
	}
	b, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#b: %v", err)
	}
	ok, aerr := a.TryAcquire(ctx, "node-a", 30*time.Second)
	if aerr != nil {
		t.Fatalf("A TryAcquire: %v", aerr)
	}
	if !ok {
		t.Fatal("无竞争时 A TryAcquire 应成功")
	}
	ok, berr := b.TryAcquire(ctx, "node-b", 30*time.Second)
	if berr != nil {
		t.Fatalf("B TryAcquire 被持有应返回 (false, nil) 而非错误: %v", berr)
	}
	if ok {
		t.Fatal("B TryAcquire 应失败（A 持有租约）——互斥语义破坏")
	}
	if relErr := a.Release(ctx, "node-a"); relErr != nil {
		t.Fatalf("A Release: %v", relErr)
	}
	ok, err2 := b.TryAcquire(ctx, "node-b", 30*time.Second)
	if err2 != nil || !ok {
		t.Fatalf("A 释放后 B 应成功获取: ok=%v err=%v", ok, err2)
	}
	_ = b.Release(ctx, "node-b")
}

// TestMongoLeaderElector_LeaseExpiry TTL 租约过期：A 获取后不续租 → 等过期 → B 可抢占
// （条件轮询 testutil.WaitFor 语义——短 ttl + 主动写旧 expires_at 加速，不 time.Sleep）。
func TestMongoLeaderElector_LeaseExpiry(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "leaseexp")
	a, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#a: %v", err)
	}
	b, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#b: %v", err)
	}
	ok, aerr := a.TryAcquire(ctx, "node-a", 30*time.Second)
	if aerr != nil || !ok {
		t.Fatalf("A TryAcquire: ok=%v err=%v", ok, aerr)
	}
	// B 抢占应失败。
	if ok, _ := b.TryAcquire(ctx, "node-b", 30*time.Second); ok {
		t.Fatal("B 在 A 持有期应抢占失败")
	}
	// 人为把租约改旧（模拟 TTL 过期——写 expires_at 为过去时间）。
	if err := expireLease(mc.URI, db, col); err != nil {
		t.Fatalf("过期租约: %v", err)
	}
	// 条件轮询：B 最终应能抢占（mongo 主从复制延迟容忍 3s 窗口）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, berr := b.TryAcquire(ctx, "node-b", 30*time.Second)
		if berr != nil {
			t.Fatalf("B 重试 TryAcquire: %v", berr)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("租约过期后 B 应能抢占（TTL 兜底失效）")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 旧主 A Renew → ErrLeaseLost（防旧主复活）。
	if rerr := a.Renew(ctx, "node-a"); !errors.Is(rerr, state.ErrLeaseLost) {
		t.Fatalf("旧主 Renew 应 ErrLeaseLost（防旧主复活）, got %v", rerr)
	}
	_ = b.Release(ctx, "node-b")
}

// TestMongoLeaderElector_RenewLost 续租丢失：A 持有 → B 抢占（过期后）→ A Renew
// ErrLeaseLost。
func TestMongoLeaderElector_RenewLost(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "renewlost")
	a, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#a: %v", err)
	}
	b, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector#b: %v", err)
	}
	ok, _ := a.TryAcquire(ctx, "node-a", 30*time.Second)
	if !ok {
		t.Fatal("A TryAcquire 应成功")
	}
	if err := expireLease(mc.URI, db, col); err != nil {
		t.Fatalf("过期租约: %v", err)
	}
	if ok, _ := b.TryAcquire(ctx, "node-b", 30*time.Second); !ok {
		t.Fatal("过期后 B 应能抢占")
	}
	if rerr := a.Renew(ctx, "node-a"); !errors.Is(rerr, state.ErrLeaseLost) {
		t.Fatalf("A Renew 应 ErrLeaseLost, got %v", rerr)
	}
}

// TestMongoLeaderElector_ReleaseIdempotent Release 幂等：未持有 Release no-op 成功。
func TestMongoLeaderElector_ReleaseIdempotent(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "relidem")
	e, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector: %v", err)
	}
	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("未持有 Release 应幂等成功: %v", err)
	}
	ok, _ := e.TryAcquire(ctx, "node-a", 30*time.Second)
	if !ok {
		t.Fatal("TryAcquire 应成功")
	}
	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("重复 Release 应幂等: %v", err)
	}
}

// TestMongoLeaderElector_ParamValidation leaseID 空 / ttl<=0 → 参数校验错误（fail-fast）。
func TestMongoLeaderElector_ParamValidation(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "param")
	e, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
	if err != nil {
		t.Fatalf("NewMongoLeaderElector: %v", err)
	}
	if _, err := e.TryAcquire(ctx, "", 30*time.Second); err == nil {
		t.Fatal("空 leaseID 应拒绝")
	}
	if _, err := e.TryAcquire(ctx, "node-a", 0); err == nil {
		t.Fatal("ttl<=0 应拒绝（防无限期持有隐式语义）")
	}
	if err := e.Renew(ctx, ""); err == nil {
		t.Fatal("空 leaseID Renew 应拒绝")
	}
}

// TestMongoLeaderElector_ConcurrentAcquire 并发抢占：多个实例同时 TryAcquire 同一
// 集合，任意时刻最多一个持有者（互斥判据：成功获取的重叠 ≤ 1）。
func TestMongoLeaderElector_ConcurrentAcquire(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "concacquire")
	const n = 6
	electors := make([]*MongoLeaderElector, n)
	for i := range n {
		e, err := NewMongoLeaderElector(mc.URI, db, col, 2*time.Second, logger)
		if err != nil {
			t.Fatalf("NewMongoLeaderElector#%d: %v", i, err)
		}
		electors[i] = e
	}
	type result struct {
		ok   bool
		used time.Duration
	}
	results := make(chan result, n)
	for i := range n {
		go func(i int) {
			start := time.Now()
			// 不同 leaseID：互斥语义必须跨身份成立（同一 leaseID 重入 = 续租语义，允许）。
			ok, err := electors[i].TryAcquire(ctx, fmt.Sprintf("node-%d", i), 5*time.Second)
			if err != nil {
				t.Errorf("并发 TryAcquire[%d]: %v", i, err)
				results <- result{}
				return
			}
			results <- result{ok: ok, used: time.Since(start)}
		}(i)
	}
	var winners []time.Duration
	for i := 0; i < n; i++ {
		r := <-results
		if r.ok {
			winners = append(winners, r.used)
		}
	}
	if len(winners) == 0 {
		t.Fatal("并发抢占应至少 1 个获胜者")
	}
	// 同一时刻（首胜者 5s ttl 窗口内）最多 1 个成功——并发发起（<1s 内）的多个
	// 获胜者即互斥破坏（不同 leaseID 必须互斥）。
	if len(winners) > 1 {
		sortDurations(winners)
		if winners[1]-winners[0] < 5*time.Second {
			t.Fatalf("并发抢占窗口内出现多个获胜者（互斥破坏）: %v", winners)
		}
	}
	for i := range n {
		_ = electors[i].Release(ctx, fmt.Sprintf("node-%d", i))
	}
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}
