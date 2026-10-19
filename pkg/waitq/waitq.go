// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package waitq 提供**通用的资源等待队列**能力（设计 docs/designs/2026-10-19-waitq.md）：
// 使用方（配额 / staging / 内存 / CPU 槽 / 带宽…）只需实现 Resource 接口，即可获得
// FIFO + 优先级 + 有界排队 + 定向唤醒 + 超时/取消 + 指标 的完整等待语义。
//
// 设计原则（用户要求 ⑥⑦⑩）：**pkg 提供通用能力抽象，使用方按场景扩展**；能力由**结构体**
// 提供（无全局可变状态，可测试、不污染其他用例）；本包**不**依赖任何具体资源实现。
//
// 关键不变式（设计 §3.1）：
//
//	I2 无惊群：唤醒从队首扫描，只唤醒「本次可用额度能满足」的等待者（有界扫描，命中即停）。
//	I3 无忙等：被唤醒后**重新尝试**获取；失败则继续排队（double-check，不自旋）。
//	I4 无死锁：多资源按 Name 字典序获取；失败按逆序回滚（原子语义）。
//	I5 有界：等待者数 ≤ Options.MaxWaiters，超限立即 ErrQueueFull（调用方映射 429）。
//	I6 无饿死：优先级有序 + 等待超过 Options.AgeBoost 后有效优先级提升。
//
// **I1（丢唤醒防护）需使用方配合**：释放侧在**完成释放之后**调用 Queue.Notify(res)
// （设计 §3.1 I1 的审核修订 —— 库不单独保证，必须由释放侧同提交接线）。
package waitq

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// 哨兵错误（调用方按需映射为 HTTP 状态：ErrQueueFull → 429 + Retry-After）。
var (
	// ErrInsufficient 表示资源当前额度不足（TryAcquire 的约定返回值）。
	ErrInsufficient = errors.New("waitq: insufficient resource")
	// ErrQueueFull 表示等待队列已满（有界，I5）。
	ErrQueueFull = errors.New("waitq: wait queue full")
	// ErrTooLarge 表示请求量**永远不可能**被满足（> 资源上限）⇒ 立即失败，不占用队列。
	ErrTooLarge = errors.New("waitq: request exceeds resource capacity")
	// ErrTimeout 表示等待超时（Request.Timeout 或 Queue 默认）。
	ErrTimeout = errors.New("waitq: wait timeout")
	// ErrClosed 表示队列已关闭。
	ErrClosed = errors.New("waitq: queue closed")
)

// Resource 是使用方实现的资源适配器（配额池 / staging 记账 / 内存 / CPU 槽…）。
type Resource interface {
	// Name 是唯一标识（多资源获取的排序键 + 指标标签）。**须登记**（设计 R36-c）。
	Name() string
	// Capacity 是当前可发放上限（含运维余量折算）；0 = 无上限（由 TryAcquire 自行判定）。
	Capacity() int64
	// Used 是当前已占用（含已预留）。
	Used() int64
	// TryAcquire 原子尝试占用 n：不足返回 ErrInsufficient；其它错误原样返回。
	TryAcquire(n int64) error
	// Release 释放 n（**不负责唤醒**；唤醒经 Queue.Notify，见 I1）。
	Release(n int64)
}

// Priority 是等待优先级（数值越大越优先）。
type Priority int

// 优先级档位。
const (
	Background Priority = iota
	Normal
	Interactive
)

// Request 是一次等待获取的请求。
type Request struct {
	// Group 是要**原子**获取的资源集合（按 Name 字典序获取，失败逆序回滚）。
	Group []Resource
	// N 是权重（字节 / 槽位…），必须 > 0。
	N int64
	// Priority 是优先级（零值 = Background；常规场景用 Normal）。
	Priority Priority
	// Timeout 是本次等待上限；0 ⇒ 用 Queue 的 DefaultWait（DefaultWait 也为 0 ⇒ 无限等）。
	Timeout time.Duration
	// Label 是指标标签（受控枚举，避免高基数）。
	Label string
	// Holder 是持有者标识（设计 W9）：同 Holder 重复获取同一资源由使用方自行检测；
	// 本包原样记录进 Stats，便于排障。
	Holder string
}

// Lease 是一次成功获取的凭据（Release 幂等）。
type Lease struct {
	once sync.Once
	got  []acquired
}

type acquired struct {
	res Resource
	n   int64
}

// Release 释放本次获取的全部资源（幂等：重复调用无副作用）。
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		// 逆序释放（与获取顺序对称）。
		for _, v := range slices.Backward(l.got) {
			v.res.Release(v.n)
		}
	})
}

// N 返回本次获取的权重（单资源场景即字节数/槽位数）。
func (l *Lease) N() int64 {
	if l == nil || len(l.got) == 0 {
		return 0
	}
	return l.got[0].n
}

// Clock 是可注入时钟（与 pkg/audit 同一契约；测试可注入假时钟，禁 time.Sleep）。
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// Metrics 是可注入指标（默认 NullMetrics；本包不依赖具体指标后端）。
type Metrics interface {
	// IncWaiters 记录当前等待者数变化（delta 可为负）。
	IncWaiters(delta int)
	// ObserveWait 记录一次等待时长与结果（ok|timeout|canceled|too_large|full|closed|error）。
	ObserveWait(d time.Duration, result string)
}

// Options 是 Queue 构造参数（全部经注入 ⇒ 无全局可变状态）。
type Options struct {
	Clock       Clock
	DefaultWait time.Duration
	MaxWaiters  int
	AgeBoost    time.Duration
	Metrics     Metrics
}

// Stats 是队列运行统计（只读快照）。
type Stats struct {
	Waiting    int           // 当前等待者数
	MaxWaiting int           // 历史最大等待者数
	Acquired   uint64        // 成功获取次数
	TimedOut   uint64        // 超时次数
	Canceled   uint64        // 取消次数
	QueueFull  uint64        // 队列满拒绝次数
	TooLarge   uint64        // 不可满足拒绝次数
	TotalWait  time.Duration // 累计等待时长（成功获取者）
}

type waiter struct {
	req Request
	ch  chan struct{}
	seq uint64
	enq time.Time
}

// Queue 是通用等待队列（**唯一入口** Acquire；**释放侧**调 Notify）。
type Queue struct {
	mu      sync.Mutex
	waiters []*waiter // 有序：有效优先级（含老化）→ 入队序
	seq     uint64
	stats   Stats
	closed  bool

	clock       Clock
	defaultWait time.Duration
	maxWaiters  int
	ageBoost    time.Duration
	metrics     Metrics
}

type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

// New 构造队列。
func New(opts Options) *Queue {
	q := &Queue{
		clock:       opts.Clock,
		defaultWait: opts.DefaultWait,
		maxWaiters:  opts.MaxWaiters,
		ageBoost:    opts.AgeBoost,
		metrics:     opts.Metrics,
	}
	if q.clock == nil {
		q.clock = realClock{}
	}
	if q.metrics == nil {
		q.metrics = NullMetrics{}
	}
	return q
}

// NullMetrics 是不记录任何指标的 Metrics 实现（默认）。
type NullMetrics struct{}

// IncWaiters 实现 Metrics。
func (NullMetrics) IncWaiters(int) {}

// ObserveWait 实现 Metrics。
func (NullMetrics) ObserveWait(time.Duration, string) {}

// Acquire 是等待获取资源的**唯一入口**：
//
//   - 先尝试（I3 double-check）；成功即返回 Lease；
//   - 请求量**永远不可能**满足（> Capacity，且 Capacity > 0）⇒ 立即 ErrTooLarge（不排队）；
//   - 否则入队等待；超时/取消/关闭返回对应错误；
//   - 被 Notify 唤醒后**重新尝试**（失败则继续等，不忙等）。
//
// 资源获取是**原子**的（多资源按 Name 排序，失败逆序回滚，I4）。
func (q *Queue) Acquire(ctx context.Context, req Request) (*Lease, error) {
	if req.N <= 0 {
		return nil, fmt.Errorf("waitq: N must be > 0, got %d", req.N)
	}
	if len(req.Group) == 0 {
		return nil, errors.New("waitq: empty resource group")
	}
	group := slices.Clone(req.Group)
	slices.SortFunc(group, func(a, b Resource) int { return strings.Compare(a.Name(), b.Name()) })
	req.Group = group

	// 不可满足 ⇒ 立即失败（不占队列、不等待）：避免「等一个永远不会来的额度」。
	if err := q.canNeverFit(req); err != nil {
		q.metrics.ObserveWait(0, "too_large")
		q.mu.Lock()
		q.stats.TooLarge++
		q.mu.Unlock()
		return nil, err
	}

	start := q.clock.Now()
	// 快速路径（I3）。
	if lease, err := q.tryAll(req); err == nil {
		q.metrics.ObserveWait(0, "ok")
		q.mu.Lock()
		q.stats.Acquired++
		q.mu.Unlock()
		return lease, nil
	} else if !errors.Is(err, ErrInsufficient) {
		return nil, err
	}

	// 慢路径：入队等待。
	w, err := q.enqueue(req)
	if err != nil {
		q.metrics.ObserveWait(q.clock.Since(start), resultFor(err))
		return nil, err
	}

	timeout := req.Timeout
	if timeout == 0 {
		timeout = q.defaultWait
	}
	return q.waitAcquire(ctx, req, w, timeout, start)
}

// waitAcquire 是 Acquire 的**慢路径**（入队后）：等待唤醒/超时/取消，被唤醒后重新尝试
// 获取（I3 无忙等）；仍不足则继续等（不重复入队、不投递重复唤醒）。拆出以控制认知复杂度。
func (q *Queue) waitAcquire(ctx context.Context, req Request, w *waiter, timeout time.Duration, start time.Time) (*Lease, error) {
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	for {
		select {
		case <-w.ch:
			lease, aerr := q.tryAll(req)
			if aerr == nil {
				q.dequeue(w)
				q.metrics.ObserveWait(q.clock.Since(start), "ok")
				q.mu.Lock()
				q.stats.Acquired++
				q.stats.TotalWait += q.clock.Since(start)
				q.mu.Unlock()
				return lease, nil
			}
			if !errors.Is(aerr, ErrInsufficient) {
				q.dequeue(w)
				q.metrics.ObserveWait(q.clock.Since(start), resultFor(aerr))
				return nil, aerr
			}
			if q.isClosed() {
				q.dequeue(w)
				q.metrics.ObserveWait(q.clock.Since(start), "closed")
				return nil, ErrClosed
			}
		case <-timeoutCh:
			q.dequeue(w)
			q.metrics.ObserveWait(q.clock.Since(start), "timeout")
			q.mu.Lock()
			q.stats.TimedOut++
			q.mu.Unlock()
			return nil, ErrTimeout
		case <-ctx.Done():
			q.dequeue(w)
			q.metrics.ObserveWait(q.clock.Since(start), "canceled")
			q.mu.Lock()
			q.stats.Canceled++
			q.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (q *Queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

// canNeverFit 报告请求是否**永远**不可能被满足（> 资源上限）⇒ 立即失败。
func (q *Queue) canNeverFit(req Request) error {
	for _, r := range req.Group {
		if limit := r.Capacity(); limit > 0 && req.N > limit {
			return fmt.Errorf("%w: %s needs %d > capacity %d", ErrTooLarge, r.Name(), req.N, limit)
		}
	}
	return nil
}

// tryAll 按序原子尝试获取整组资源；任一步失败即**逆序回滚**已获取部分（I4）。
func (q *Queue) tryAll(req Request) (*Lease, error) {
	got := make([]acquired, 0, len(req.Group))
	for _, r := range req.Group {
		if err := r.TryAcquire(req.N); err != nil {
			for _, g := range slices.Backward(got) {
				g.res.Release(g.n)
			}
			return nil, err
		}
		got = append(got, acquired{res: r, n: req.N})
	}
	return &Lease{got: got}, nil
}

// enqueue 入队（有界：超限立即 ErrQueueFull，I5）。
func (q *Queue) enqueue(req Request) (*waiter, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if q.maxWaiters > 0 && len(q.waiters) >= q.maxWaiters {
		q.stats.QueueFull++
		return nil, ErrQueueFull
	}
	q.seq++
	w := &waiter{req: req, ch: make(chan struct{}, 1), seq: q.seq, enq: q.clock.Now()}
	q.waiters = append(q.waiters, w)
	q.sortWaitersLocked()
	q.stats.Waiting = len(q.waiters)
	if len(q.waiters) > q.stats.MaxWaiting {
		q.stats.MaxWaiting = len(q.waiters)
	}
	q.metrics.IncWaiters(1)
	return w, nil
}

// dequeue 出队（幂等：不存在时无操作）。
func (q *Queue) dequeue(w *waiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.waiters {
		if x == w {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			q.stats.Waiting = len(q.waiters)
			q.metrics.IncWaiters(-1)
			return
		}
	}
}

// effectivePriority 返回含**老化提升**的有效优先级（I6；调用方须持 q.mu）。
func (q *Queue) effectivePriority(w *waiter) int {
	p := int(w.req.Priority)
	if q.ageBoost > 0 && q.clock.Since(w.enq) >= q.ageBoost {
		p++
	}
	return p
}

// sortWaitersLocked 按（有效优先级 desc，入队序 asc）排序（调用方须持 q.mu）。
func (q *Queue) sortWaitersLocked() {
	slices.SortStableFunc(q.waiters, func(a, b *waiter) int {
		pa, pb := q.effectivePriority(a), q.effectivePriority(b)
		if pa != pb {
			return pb - pa // 高优先级在前
		}
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		default:
			return 0
		}
	})
}

// canSatisfyLocked 判断当前额度是否可能满足该等待者（I2 的唤醒判据；须持 q.mu）。
func (q *Queue) canSatisfyLocked(w *waiter) bool {
	for _, r := range w.req.Group {
		if limit := r.Capacity(); limit > 0 && limit-r.Used() < w.req.N {
			return false
		}
	}
	return true
}

// Notify 是**释放侧的唤醒入口**（I1）：在完成释放之后调用。
//
// 从队首扫描，只唤醒「本次可用额度能满足」的等待者（有界扫描，命中即停）⇒ 无惊群（I2）。
// 被唤醒者会自行重新尝试获取（I3）；失败则继续排队。
func (q *Queue) Notify(res Resource) {
	if res == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	// 唤醒前重排：老化（I6）随等待时间推进生效（否则排序只在入队时发生）。
	q.sortWaitersLocked()
	name := res.Name()
	for _, w := range q.waiters {
		if !groupHas(w.req.Group, name) {
			continue
		}
		if !q.canSatisfyLocked(w) {
			continue // 额度仍不够：不唤醒（等下一次释放）
		}
		select {
		case w.ch <- struct{}{}:
		default: // 已有待处理唤醒：不重复投递
		}
		return // 命中即停（有界扫描；其余等待者由后续 Notify 覆盖）
	}
}

// NotifyAll 唤醒所有「额度能满足」的等待者（用于大幅放宽额度/运维调额的场景）。
func (q *Queue) NotifyAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.sortWaitersLocked()
	for _, w := range q.waiters {
		if !q.canSatisfyLocked(w) {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

// Stats 返回统计快照。
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

// Waiting 返回当前等待者数。
func (q *Queue) Waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}

// Close 关闭队列：在途等待者收到错误（ErrClosed），后续 Acquire 直接失败。
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	waiters := q.waiters
	q.waiters = nil
	q.stats.Waiting = 0
	q.mu.Unlock()
	q.metrics.IncWaiters(-len(waiters))
	for _, w := range waiters {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

func groupHas(g []Resource, name string) bool {
	for _, r := range g {
		if r.Name() == name {
			return true
		}
	}
	return false
}

func resultFor(err error) string {
	switch {
	case errors.Is(err, ErrQueueFull):
		return "full"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, ErrClosed):
		return "closed"
	default:
		return "error"
	}
}
