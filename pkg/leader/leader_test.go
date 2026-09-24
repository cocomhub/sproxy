// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package leader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// newLocalPair 返回同一 root 下的两个 LocalLeaderElector 实例（同目录两把句柄，
// 用于互斥语义断言——flock/LockFileEx 必须跨句柄互斥）。
func newLocalPair(t *testing.T) (*LocalLeaderElector, *LocalLeaderElector, string) {
	t.Helper()
	root := t.TempDir()
	a := NewLocalLeaderElector(root)
	b := NewLocalLeaderElector(root)
	return a, b, root
}

// TestLocalLeaderElector_AcquireRelease 核心往返：TryAcquire 成功 → Renew 成功 →
// Release → 可再 Acquire。
func TestLocalLeaderElector_AcquireRelease(t *testing.T) {
	t.Parallel()
	e, _, root := newLocalPair(t)
	ctx := context.Background()

	ok, aerr := e.TryAcquire(ctx, "node-a", 30*time.Second)
	if aerr != nil {
		t.Fatalf("TryAcquire: %v", aerr)
	}
	if !ok {
		t.Fatalf("无竞争时 TryAcquire 应恒 true（单节点恒主零回归），root=%s", root)
	}
	if rerr := e.Renew(ctx, "node-a"); rerr != nil {
		t.Fatalf("Renew 应成功: %v", rerr)
	}
	if relErr := e.Release(ctx, "node-a"); relErr != nil {
		t.Fatalf("Release: %v", relErr)
	}
	// 释放后锁文件应仍存在（不删除——诊断文件，后续可再获取）。
	if _, statErr := os.Stat(filepath.Join(root, "state", "leader.lock")); statErr != nil {
		t.Fatalf("释放后锁文件应保留（诊断用途）: %v", statErr)
	}
	ok, aerr = e.TryAcquire(ctx, "node-a", 30*time.Second)
	if aerr != nil || !ok {
		t.Fatalf("释放后应可再次获取: ok=%v err=%v", ok, aerr)
	}
	_ = e.Release(ctx, "node-a")
}

// TestLocalLeaderElector_SecondAcquireFails 核心互斥语义：A 持有 → B TryAcquire
// (false, nil)；A Release → B 成功。
func TestLocalLeaderElector_SecondAcquireFails(t *testing.T) {
	t.Parallel()
	a, b, _ := newLocalPair(t)
	ctx := context.Background()

	ok, aerr := a.TryAcquire(ctx, "node-a", 30*time.Second)
	if aerr != nil || !ok {
		t.Fatalf("A TryAcquire: ok=%v err=%v", ok, aerr)
	}
	ok, berr := b.TryAcquire(ctx, "node-b", 30*time.Second)
	if berr != nil {
		t.Fatalf("B TryAcquire 被持有应返回 (false, nil) 而非错误: %v", berr)
	}
	if ok {
		t.Fatal("B TryAcquire 应失败（A 已持有排他锁）——互斥语义破坏")
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

// TestLocalLeaderElector_ReleaseIdempotent 重复 Release 不报错（幂等）。
func TestLocalLeaderElector_ReleaseIdempotent(t *testing.T) {
	t.Parallel()
	e, _, _ := newLocalPair(t)
	ctx := context.Background()

	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("未持有的 Release 应幂等成功: %v", err)
	}
	ok, err := e.TryAcquire(ctx, "node-a", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("TryAcquire: ok=%v err=%v", ok, err)
	}
	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := e.Release(ctx, "node-a"); err != nil {
		t.Fatalf("重复 Release 应幂等: %v", err)
	}
}

// TestLocalLeaderElector_ParamValidation leaseID 空 / ttl<=0 → 参数校验错误（fail-fast）。
func TestLocalLeaderElector_ParamValidation(t *testing.T) {
	t.Parallel()
	e, _, _ := newLocalPair(t)
	ctx := context.Background()

	if _, err := e.TryAcquire(ctx, "", 30*time.Second); err == nil {
		t.Fatal("空 leaseID 应拒绝")
	}
	if _, err := e.TryAcquire(ctx, "node-a", 0); err == nil {
		t.Fatal("ttl<=0 应拒绝（防无限期持有隐式语义）")
	}
	if _, err := e.TryAcquire(ctx, "node-a", -time.Second); err == nil {
		t.Fatal("负 ttl 应拒绝")
	}
	if err := e.Renew(ctx, ""); err == nil {
		t.Fatal("空 leaseID Renew 应拒绝")
	}
}

// fakeElector 是测试用内存选主（WriteGuard 续租循环注入点）。
// 行为：TryAcquire 按 acquired flag 决定成功/失败；Renew 按 renewErr 返回；
// Release 幂等。全部调用计数可观测。
type fakeElector struct {
	acquired atomic.Bool
	mu       sync.Mutex
	renewErr error
	acquires atomic.Int64
	renews   atomic.Int64
}

func newFakeElector() *fakeElector {
	return &fakeElector{}
}

func (f *fakeElector) setRenewErr(err error) {
	f.mu.Lock()
	f.renewErr = err
	f.mu.Unlock()
}

func (f *fakeElector) renewErrValue() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewErr
}

func (f *fakeElector) TryAcquire(_ context.Context, _ string, _ time.Duration) (bool, error) {
	f.acquires.Add(1)
	if !f.acquired.Load() {
		return false, nil
	}
	// 抢占成功后租约归本节点持有：续租应恢复成功（直到再次丢失）。
	f.setRenewErr(nil)
	return true, nil
}

func (f *fakeElector) Renew(_ context.Context, _ string) error {
	f.renews.Add(1)
	if err := f.renewErrValue(); err != nil {
		return err
	}
	return nil
}

func (f *fakeElector) Release(_ context.Context, _ string) error { return nil }

// TestWriteGuard_NotLeader isLeader=false → Authorize 返回 ErrNotLeader；true → nil。
func TestWriteGuard_NotLeader(t *testing.T) {
	t.Parallel()
	g := NewWriteGuard(newFakeElector(), "node-a", testutil.DiscardLogger())
	if err := g.Authorize(); err == nil {
		t.Fatal("isLeader=false 时 Authorize 应拒绝")
	}
	if !errors.Is(g.Authorize(), ErrNotLeader) {
		t.Fatalf("应返回 ErrNotLeader，实际 %v", g.Authorize())
	}
	g.SetLeader(true)
	if err := g.Authorize(); err != nil {
		t.Fatalf("isLeader=true 时 Authorize 应放行: %v", err)
	}
}

// TestWriteGuard_NilElector_ZeroRegression 未装配 elector（nil）→ 恒主放行（零回归）。
func TestWriteGuard_NilElector_ZeroRegression(t *testing.T) {
	t.Parallel()
	g := NewWriteGuard(nil, "node-a", testutil.DiscardLogger())
	if err := g.Authorize(); err != nil {
		t.Fatalf("未装配 WriteGuard 应恒放行（旧行为零回归）: %v", err)
	}
}

// TestWriteGuard_RenewLoop_Failover 主节点故障 → RenewLoop 补位 → isLeader 翻转为 true。
// fake elector：开始 acquired=true 且 renew 失败（模拟旧主租约丢失）→ 退避重试
// acquired=true → isLeader=true。条件轮询断言（不 time.Sleep——R14）。
func TestWriteGuard_RenewLoop_Failover(t *testing.T) {
	t.Parallel()
	f := newFakeElector()
	f.acquired.Store(true)
	f.setRenewErr(ErrLeaseLost)
	g := NewWriteGuard(f, "node-a", testutil.DiscardLogger())

	ctx := t.Context()
	go g.RenewLoop(ctx, "node-a", 10*time.Millisecond, 30*time.Millisecond)

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return g.IsLeader()
	}, "续租循环应在退避重试后重新成为主节点")

	if f.renews.Load() == 0 {
		t.Fatal("RenewLoop 应调用 elector.Renew")
	}
	if f.acquires.Load() == 0 {
		t.Fatal("Renew 失败后应重试 TryAcquire")
	}
}

// TestWriteGuard_RenewLoop_LossDetected Renew 返回 ErrLeaseLost → isLeader 翻转为 false
// （fail-closed：旧主不得继续写）。
func TestWriteGuard_RenewLoop_LossDetected(t *testing.T) {
	t.Parallel()
	f := newFakeElector()
	f.acquired.Store(false) // 补位失败（被新主持有）
	f.setRenewErr(ErrLeaseLost)
	g := NewWriteGuard(f, "node-a", testutil.DiscardLogger())
	g.SetLeader(true)

	ctx := t.Context()
	go g.RenewLoop(ctx, "node-a", 5*time.Millisecond, 15*time.Millisecond)

	testutil.WaitFor(t, 5*time.Second, func() bool {
		return !g.IsLeader()
	}, "租约丢失后 isLeader 必须翻转为 false（fail-closed，禁继续写）")
}

// TestLocalLeaderElector_ConcurrentAcquire 并发抢占：多个 goroutine 同时 TryAcquire
// 同一 path，任意时刻最多一个持有者重叠（互斥 + 无 panic/死锁）。
// 注意：各 goroutine 获取后立即释放，**顺序**成功是合法行为（互斥只约束重叠）；
// 断言「恰好 1 个获胜者」会把顺序成功误判为破坏——正确不变量是 holder 计数峰值 ≤ 1。
func TestLocalLeaderElector_ConcurrentAcquire(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ctx := context.Background()
	const n = 8
	electors := make([]*LocalLeaderElector, n)
	for i := range n {
		electors[i] = NewLocalLeaderElector(root)
	}

	var (
		holders   atomic.Int64 // 当前同时持有者数
		maxHolder atomic.Int64 // 峰值（互斥判据：必须 ≤ 1）
		winners   atomic.Int64
		wg        sync.WaitGroup
	)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := electors[i].TryAcquire(ctx, "node-x", 30*time.Second)
			if err != nil {
				t.Errorf("并发 TryAcquire[%d]: %v", i, err)
				return
			}
			if ok {
				winners.Add(1)
				if cur := holders.Add(1); cur > maxHolder.Load() {
					maxHolder.Store(cur)
				}
				holders.Add(-1)
			}
			_ = electors[i].Release(ctx, "node-x")
		}(i)
	}
	wg.Wait()
	if got := winners.Load(); got < 1 {
		t.Fatalf("并发抢占应至少 1 个获胜者，实际 %d", got)
	}
	if got := maxHolder.Load(); got > 1 {
		t.Fatalf("并发抢占任意时刻最多 1 个持有者，实测峰值 %d（互斥语义破坏）", got)
	}
}

// TestWriteGuard_RenewLoop_CtxCancel ctx 取消后循环退出（不泄漏 goroutine）。
func TestWriteGuard_RenewLoop_CtxCancel(t *testing.T) {
	t.Parallel()
	f := newFakeElector()
	f.acquired.Store(true)
	g := NewWriteGuard(f, "node-a", testutil.DiscardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.RenewLoop(ctx, "node-a", 5*time.Millisecond, 15*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 RenewLoop 应退出")
	}
}
