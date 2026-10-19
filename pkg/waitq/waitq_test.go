// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package waitq

// waitq_test.go 锁定设计 docs/designs/2026-10-19-waitq.md §3.1 的关键不变式：
// I2 无惊群 / I3 无忙等 / I4 无死锁（原子 + 回滚）/ I5 有界 / I6 优先级。
//
// 纪律：**不使用 time.Sleep**（R14 棘轮）—— 同步一律用 channel（测试内同步通道）；
// 超时用例用极短的真实超时（10ms 级）而非 sleep 轮询。

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// fakeRes 是测试用资源适配器（容量固定 + 线程安全占用计数）。
type fakeRes struct {
	name string
	cap  int64

	mu   sync.Mutex
	used int64
	// acquires / releases 记录调用次数（断言回滚：失败路径不得留下占用）。
	acquires int
	releases int
}

func newFakeRes(name string, capacity int64) *fakeRes {
	return &fakeRes{name: name, cap: capacity}
}

func (r *fakeRes) Name() string { return r.name }

func (r *fakeRes) Capacity() int64 { return r.cap }

func (r *fakeRes) Used() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.used
}

func (r *fakeRes) TryAcquire(n int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap > 0 && r.used+n > r.cap {
		return ErrInsufficient
	}
	r.used += n
	r.acquires++
	return nil
}

func (r *fakeRes) Release(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.used -= n
	r.releases++
}

// TestAcquire_FastPath 额度充足：立即获取（不入队），Release 幂等。
func TestAcquire_FastPath(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4})

	lease, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 40, Priority: Normal})
	if err != nil {
		t.Fatalf("额度充足应立即获取: %v", err)
	}
	if got := res.Used(); got != 40 {
		t.Fatalf("占用应为 40, got %d", got)
	}
	if q.Waiting() != 0 {
		t.Fatalf("快速路径不应入队, waiting=%d", q.Waiting())
	}
	lease.Release()
	lease.Release() // 幂等：不得重复释放
	if got := res.Used(); got != 0 {
		t.Fatalf("释放后占用应为 0, got %d", got)
	}
	if res.releases != 1 {
		t.Fatalf("Release 必须幂等（只释放一次）, got %d 次", res.releases)
	}
	if s := q.Stats(); s.Acquired != 1 {
		t.Fatalf("Acquired 应为 1, got %d", s.Acquired)
	}
}

// TestAcquire_WaitsThenNotified 额度不足 ⇒ 入队等待；释放 + Notify ⇒ 唤醒并获取（I1/I2）。
func TestAcquire_WaitsThenNotified(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4, DefaultWait: 2 * time.Second})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}

	done := make(chan *Lease, 1)
	go func() {
		l, aerr := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 60, Priority: Normal})
		if aerr != nil {
			t.Errorf("等待者应成功获取: %v", aerr)
		}
		done <- l
	}()

	// 等待者必须**真的**在队列里（而非忙等返回错误）。
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if q.Waiting() != 1 {
		t.Fatalf("等待者应入队, waiting=%d", q.Waiting())
	}

	// 释放 + **Notify**（释放侧统一入口，I1）。
	hold.Release()
	q.Notify(res)

	select {
	case l := <-done:
		if l == nil {
			t.Fatal("应获得 lease")
		}
		if got := res.Used(); got != 60 {
			t.Fatalf("唤醒后占用应为 60, got %d", got)
		}
		l.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("Notify 后应被唤醒（丢唤醒 = I1 违约）")
	}
	if q.Waiting() != 0 {
		t.Fatalf("获取后应出队, waiting=%d", q.Waiting())
	}
}

// TestAcquire_TooLargeFailsImmediately 请求量 > 上限 ⇒ 立即 ErrTooLarge（不排队，I5/设计 §4）。
func TestAcquire_TooLargeFailsImmediately(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4})

	_, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 101, Priority: Normal})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过上限应 ErrTooLarge, got %v", err)
	}
	if q.Waiting() != 0 {
		t.Fatalf("不可满足的请求不得占用队列, waiting=%d", q.Waiting())
	}
	if res.acquires != 0 || res.Used() != 0 {
		t.Fatalf("不可满足不得触碰资源, acquires=%d used=%d", res.acquires, res.Used())
	}
	if s := q.Stats(); s.TooLarge != 1 {
		t.Fatalf("TooLarge 计数应为 1, got %d", s.TooLarge)
	}
}

// TestAcquire_QueueFull 队列有界（I5）：超限立即 ErrQueueFull（调用方映射 429）。
func TestAcquire_QueueFull(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 1, DefaultWait: 2 * time.Second})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	defer hold.Release()

	first := make(chan struct{})
	go func() {
		_, _ = q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 10, Priority: Normal})
		close(first)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}

	_, err = q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 10, Priority: Normal})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("队列满应 ErrQueueFull, got %v", err)
	}
	if s := q.Stats(); s.QueueFull != 1 {
		t.Fatalf("QueueFull 计数应为 1, got %d", s.QueueFull)
	}
	// 收尾：唤醒在途等待者，避免测试泄漏 goroutine。
	hold.Release()
	q.Notify(res)
	<-first
}

// TestAcquire_Timeout 超时（Request.Timeout）⇒ ErrTimeout（I5 的等待侧上界）。
func TestAcquire_Timeout(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	defer hold.Release()

	start := time.Now()
	_, err = q.Acquire(context.Background(), Request{
		Group: []Resource{res}, N: 10, Priority: Normal, Timeout: 20 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应 ErrTimeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("超时应在 20ms 量级返回, took %v", time.Since(start))
	}
	if q.Waiting() != 0 {
		t.Fatalf("超时后应出队, waiting=%d", q.Waiting())
	}
	if s := q.Stats(); s.TimedOut != 1 {
		t.Fatalf("TimedOut 计数应为 1, got %d", s.TimedOut)
	}
}

// TestAcquire_ContextCanceled 取消 ⇒ 返回 ctx.Err()（并出队，不泄漏等待者）。
func TestAcquire_ContextCanceled(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4, DefaultWait: 5 * time.Second})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	defer hold.Release()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, aerr := q.Acquire(ctx, Request{Group: []Resource{res}, N: 10, Priority: Normal})
		errCh <- aerr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	cancel()

	select {
	case aerr := <-errCh:
		if !errors.Is(aerr, context.Canceled) {
			t.Fatalf("应返回 context.Canceled, got %v", aerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后应立即返回")
	}
	if q.Waiting() != 0 {
		t.Fatalf("取消后应出队, waiting=%d", q.Waiting())
	}
}

// TestAcquire_MultiResourceAtomicRollback 多资源**原子**获取：任一步失败 ⇒ 已获取部分
// **逆序回滚**（断言「无副作用」：已成功的第一资源占用必须回到 0，I4）。
func TestAcquire_MultiResourceAtomicRollback(t *testing.T) {
	t.Parallel()
	a := newFakeRes("a-quota", 100)
	b := newFakeRes("b-capacity", 50) // 上限 50 ⇒ N=60 永远不可满足（先命中 canNeverFit）
	q := New(Options{MaxWaiters: 4})

	_, err := q.Acquire(context.Background(), Request{Group: []Resource{a, b}, N: 60, Priority: Normal})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("应 ErrTooLarge, got %v", err)
	}
	if a.Used() != 0 || b.Used() != 0 {
		t.Fatalf("失败路径不得留下占用: a=%d b=%d", a.Used(), b.Used())
	}

	// 真正触发回滚：b 有上限但 N 不超它 —— 先用满 b，再请求 a+b ⇒ a 成功、b 不足 ⇒ a 回滚。
	fill := newFakeRes("b-capacity", 50)
	if err := fill.TryAcquire(50); err != nil {
		t.Fatalf("预占 b: %v", err)
	}
	// 注意：DefaultWait=0 表示**无限等**（设计语义）⇒ 用例必须显式超时，否则会挂住。
	_, qerr := q.Acquire(context.Background(), Request{
		Group: []Resource{a, fill}, N: 10, Priority: Normal, Timeout: 30 * time.Millisecond,
	})
	if !errors.Is(qerr, ErrTimeout) {
		t.Fatalf("b 持续不足应超时（先经历一次 a 获取+b 失败的回滚）, got %v", qerr)
	}
	if a.Used() != 0 {
		t.Fatalf("a 必须被回滚（原子语义）, used=%d", a.Used())
	}
	if a.releases != a.acquires {
		t.Fatalf("回滚应释放已获取部分: acquires=%d releases=%d", a.acquires, a.releases)
	}
}

// TestAcquire_PriorityOrder 优先级：额度释放后**高优先级**先获得（I6）。
func TestAcquire_PriorityOrder(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4, DefaultWait: 2 * time.Second})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}

	order := make(chan string, 2)
	go func() {
		if _, aerr := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Background}); aerr == nil {
			order <- "background"
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() < 1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	go func() {
		if _, aerr := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Interactive}); aerr == nil {
			order <- "interactive"
		}
	}()
	for q.Waiting() < 2 && time.Now().Before(deadline) {
		runtime.Gosched()
	}

	// 释放全部 + **Notify**（命中即停 = I2 无惊群）⇒ 只唤醒队首（Interactive），确定性可断言。
	// 注：NotifyAll 会同时唤醒两个等待者，由调度器决定谁先拿到 ⇒ 不适合断言优先级顺序。
	hold.Release()
	q.Notify(res)

	select {
	case got := <-order:
		if got != "interactive" {
			t.Fatalf("应先唤醒高优先级, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("应至少有一个等待者被唤醒")
	}
}

// TestQueue_Close 关闭队列：在途等待者收到错误，后续 Acquire 立即失败。
func TestQueue_Close(t *testing.T) {
	t.Parallel()
	res := newFakeRes("quota", 100)
	q := New(Options{MaxWaiters: 4, DefaultWait: 5 * time.Second})

	hold, err := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("首次获取: %v", err)
	}
	defer hold.Release()

	errCh := make(chan error, 1)
	go func() {
		_, aerr := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 10, Priority: Normal})
		errCh <- aerr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Waiting() == 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}

	q.Close()
	select {
	case aerr := <-errCh:
		if aerr == nil {
			t.Fatal("关闭后等待者应收到错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("关闭后等待者应立即返回")
	}
	if _, aerr := q.Acquire(context.Background(), Request{Group: []Resource{res}, N: 1}); aerr == nil {
		t.Fatal("关闭后 Acquire 应失败")
	}
}

// TestQueue_Isolation 隔离性（用户要求 ④）：两个队列各自 New() ⇒ 互不影响。
func TestQueue_Isolation(t *testing.T) {
	t.Parallel()
	res1 := newFakeRes("quota", 100)
	res2 := newFakeRes("quota", 100)
	q1 := New(Options{MaxWaiters: 1})
	q2 := New(Options{MaxWaiters: 1})

	h1, err := q1.Acquire(context.Background(), Request{Group: []Resource{res1}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("q1 获取: %v", err)
	}
	defer h1.Release()
	// q2 完全不受 q1 状态影响（无全局可变状态）。
	h2, err := q2.Acquire(context.Background(), Request{Group: []Resource{res2}, N: 100, Priority: Normal})
	if err != nil {
		t.Fatalf("q2 应独立可用: %v", err)
	}
	defer h2.Release()
	if q1.Waiting() != 0 || q2.Waiting() != 0 {
		t.Fatalf("两队列均不应有等待者: q1=%d q2=%d", q1.Waiting(), q2.Waiting())
	}
}
