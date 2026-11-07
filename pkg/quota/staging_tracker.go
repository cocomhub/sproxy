// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// staging_tracker.go 实现**独立 staging 配额记账 + 排队等待**（用户裁定 2026-10-10：
// 本地上传暂存独立 Scope，不与网盘 owner_quotas 混用；本地磁盘不足排队等待而非立即
// 拒绝，永久不足（无并发释放）有界超时快速失败为单文件错误）。
//
// 供装配层（pkg/server stagingQuotaTrackerFor / cmd/sproxy baidupcs_sync）共用——逻辑
// 单一实现，不重复。实现 StagingQuotaTracker 语义：
//
//	ReserveUsage(ctx, size) → scope.TryReserve(size) + res.Commit(size) // 入账 committed
//	                         不足 → 排队等待（pkg/waitq；Release 唤醒 / ctx.Done / 超时中断）
//	ReleaseUsage(size)     → scope.ReleaseUsage(size) + queue.NotifyAll() // 唤醒等待者
//
// **等待队列已收敛到通用能力包 pkg/waitq**（设计 docs/designs/2026-10-19-waitq.md §8）：
// 私有 `sync.Cond` + 观察者 goroutine（ctx/超时打断）全部删除 —— 超时/取消由 waitq 的
// select + timer 承担（**无常驻 goroutine**），丢唤醒由「锁协作入队」结构性闭合
// （`AcquireUnder` 在 `q.mu` 内完成「查 + 入队」，与 `ReleaseUsage` 的「改账 + 唤醒」
// 串行化在同一把锁上）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// stagingWaitTimeout 是 staging 配额排队等待超时（本地磁盘不足等待释放的最长时限）。
// **瞬时磁盘紧张**（并发任务占满、释放后继续）在此时限内重试成功；**永久不足**
// （单文件超 staging 配额、无并发释放）超过此时限快速失败为单文件 ActionError——
// 排队不无限挂起，任务不卡 syncing。5s 足够瞬时释放窗口，且远小于任务/测试等待窗口。
const stagingWaitTimeout = 5 * time.Second

// StagingTracker 是 staging 配额记账 + 排队等待实现（基于独立 quota.Scope）。
type StagingTracker struct {
	scope *Scope
	// mu 是**等待协调锁**：等待侧的「查 + 入队」（waitq.AcquireUnder 的 lock）与释放侧的
	// 「改账 + 唤醒」都在它之内 ⇒ 丢唤醒窗口结构性闭合（评审 F4 的保证以锁协作形式保留）。
	mu    sync.Mutex
	queue *waitq.Queue
	// waitTimeout 是排队等待超时（<=0 = stagingWaitTimeout）；测试可注入缩短。
	waitTimeout time.Duration
	// waitRounds 是**「确认不足」的轮次**（测试可观测：替代固定 Sleep 做时序同步）：
	// 首次不足即入队等待（计 1），此后每次被唤醒重试仍不足再计 1 —— 与迁移前
	// 「每次进入 cond.Wait 计一轮」在时序同步上等价（每次等待前必有一次不足判定）。
	waitRounds atomic.Int64
	// watchers 是**观察者 goroutine 计数**：waitq 以 select + timer 实现超时/取消，
	// **不再有观察者 goroutine** ⇒ 恒为 0（比迁移前「每次调用至多 1 个」更强的保证）。
	// 保留字段是为了让既有回归用例（m7）继续断言「有界」这一不变量。
	watchers atomic.Int64
	// beforeWait 是**测试钩子**（生产恒 nil）：在「确认不足、即将进入等待」的窗口点调用
	// （**锁外**，仅用于把「释放恰在 TryReserve 失败与入队之间发生」确定化）。
	// 锁协作入队后，该窗口内的释放**不可能**被丢弃（等待者的快速尝试在锁内必然看到）。
	beforeWait func()
}

// NewStagingTracker 构造 staging 配额记账器（scope 为独立 staging Scope）。
func NewStagingTracker(scope *Scope) *StagingTracker {
	q := &StagingTracker{scope: scope, waitTimeout: stagingWaitTimeout}
	q.queue = waitq.New(waitq.Options{
		DefaultWait: stagingWaitTimeout,
		AgeBoost:    stagingWaitTimeout,
		Metrics:     stagingMetrics{tracker: q},
	})
	return q
}

// stagingResource 是 staging Scope 的 waitq.Resource 适配器：TryAcquire = TryReserve +
// Commit（入账 committed，语义与迁移前一致）；Release = ReleaseUsage（回滚路径）。
type stagingResource struct {
	scope   *Scope
	tracker *StagingTracker
}

// Name 实现 waitq.Resource。
func (r stagingResource) Name() string {
	if r.scope == nil || r.scope.pool == nil {
		return "staging:unknown"
	}
	return "staging:" + r.scope.pool.waitqPath()
}

// Capacity 实现 waitq.Resource：staging scope 的可用上限（0 = 无上限）。
func (r stagingResource) Capacity() int64 {
	if r.scope == nil || r.scope.pool == nil {
		return 0
	}
	limit, _ := r.scope.pool.waitqLimits()
	return limit
}

// Used 实现 waitq.Resource：`Capacity() - 剩余可用`。
func (r stagingResource) Used() int64 {
	if r.scope == nil || r.scope.pool == nil {
		return 0
	}
	limit, free := r.scope.pool.waitqLimits()
	return limit - free
}

// TryAcquire 实现 waitq.Resource：预留并**立即提交**（staging 语义 = 占用已确认）；
// 不足返回 waitq.ErrInsufficient 触发排队。
func (r stagingResource) TryAcquire(n int64) error {
	res, err := r.scope.TryReserve(n)
	if err != nil {
		if errors.Is(err, ErrStorageFull) {
			// **不足即计一轮**（waitRounds 的既有语义：每次「确认不足」为一轮 —— 首次不足
			// 即入队等待，此后每次被唤醒重试仍不足再计一轮）。既有回归用例据此做确定性同步。
			if r.tracker != nil {
				r.tracker.waitRounds.Add(1)
			}
			return waitq.ErrInsufficient
		}
		return err
	}
	res.Commit(n)
	return nil
}

// Release 实现 waitq.Resource：**仅回滚/放弃路径**生效（正常成功路径由调用方持有已提交额度）。
func (r stagingResource) Release(n int64) {
	r.scope.ReleaseUsage(n)
}

// stagingMetrics 把 waitq 的等待者变化接到 waitRounds（保留既有可观测契约）。
type stagingMetrics struct{ tracker *StagingTracker }

// IncWaiters 实现 waitq.Metrics：**不**在此计数（轮次由适配器在「不足」时计，
// 语义见 waitRounds 注释；两处都计会双重计数）。
func (m stagingMetrics) IncWaiters(int) {}

// ObserveWait 实现 waitq.Metrics（无指标后端接入；保留扩展点）。
func (m stagingMetrics) ObserveWait(_ time.Duration, _ string) {}

// ReserveUsage 预留 size 字节（TryReserve + Commit）；不足则**排队等待**（释放唤醒 /
// ctx 取消 / 超时）。超时或取消返回可识别错误（装配层映射为单文件 ActionError）。
func (q *StagingTracker) ReserveUsage(ctx context.Context, size int64) error {
	if size <= 0 {
		return nil
	}
	timeout := q.waitTimeout
	if timeout <= 0 {
		timeout = stagingWaitTimeout
	}
	if q.beforeWait != nil {
		q.beforeWait() // 测试钩子：窗口点（锁外；锁协作入队后该窗口的释放不会被丢弃）
	}
	// 锁协作入队：等待侧「查 + 入队」与 ReleaseUsage「改账 + 唤醒」同锁（q.mu）⇒ 无丢唤醒。
	_, err := q.queue.AcquireUnder(ctx, waitq.Request{
		Group:    []waitq.Resource{stagingResource{scope: q.scope, tracker: q}},
		N:        size,
		Priority: waitq.Normal,
		Timeout:  timeout,
		Label:    "staging",
	}, &q.mu)
	if err == nil {
		return nil // 适配器已 TryReserve + Commit（额度已入账）
	}
	switch {
	case errors.Is(err, waitq.ErrTimeout):
		// deadline 后仍不足：释放可能与超时同时发生 ⇒ 交由调用方按单文件错误处理（与迁移前同文案）。
		return fmt.Errorf("quota: 等待本地 staging 空间超时（%s）", timeout)
	case errors.Is(err, waitq.ErrTooLarge):
		// 不可能容纳（> staging 配额）⇒ **立即失败**（迁移前需白等满超时；此处为改进，非能力丢失）。
		return fmt.Errorf("quota: 本地 staging 配额不足（请求 %d 字节超过上限，即使全部释放也无法容纳）", size)
	case errors.Is(err, waitq.ErrQueueFull):
		return fmt.Errorf("quota: 本地 staging 等待队列已满: %w", err)
	case ctx.Err() != nil:
		return fmt.Errorf("quota: 等待本地 staging 空间中断: %w", ctx.Err())
	default:
		return err
	}
}

// ReleaseUsage 释放 size 字节（上传成功或失败后调用；唤醒排队等待者）。
//
// 顺序要点（与 Pool.withWaitLock 同型）：
//  1. 在 q.mu 内**改账**（与等待侧的「查 + 入队」同锁 ⇒ 丢唤醒窗口闭合）；
//  2. 在**释放 q.mu 之后**唤醒：队列的「额度是否够」判据要读账本（scope 的 pool.mu），
//     持 q.mu 唤醒虽不直接冲突，但保持「唤醒不持任何本层锁」的一致纪律更安全。
func (q *StagingTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.mu.Lock()
	q.scope.ReleaseUsage(size)
	q.mu.Unlock()
	if q.queue != nil {
		q.queue.NotifyAll()
	}
}
