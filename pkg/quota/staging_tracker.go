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
//	                         不足 → 等待（cond；Release 广播 / ctx.Done / 超时中断）
//	ReleaseUsage(size)     → scope.ReleaseUsage(size) + cond.Broadcast() // 唤醒等待者

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// stagingWaitTimeout 是 staging 配额排队等待超时（本地磁盘不足等待释放的最长时限）。
// **瞬时磁盘紧张**（并发任务占满、释放后继续）在此时限内重试成功；**永久不足**
// （单文件超 staging 配额、无并发释放）超过此时限快速失败为单文件 ActionError——
// 排队不无限挂起，任务不卡 syncing。5s 足够瞬时释放窗口，且远小于任务/测试等待窗口。
const stagingWaitTimeout = 5 * time.Second

// StagingTracker 是 staging 配额记账 + 排队等待实现（基于独立 quota.Scope）。
// 不足时排队（sync.Cond 等待 Release 广播 / ctx 取消 / 超时）；不依赖具体配额池类型。
type StagingTracker struct {
	scope *Scope
	cond  *sync.Cond // 排队等待：Release 广播唤醒
	// waitTimeout 是排队等待超时（<=0 = stagingWaitTimeout）；测试可注入缩短。
	waitTimeout time.Duration
	// waitRounds 是**进入 cond.Wait 的次数**（测试可观测：替代固定 Sleep 做时序同步）。
	waitRounds atomic.Int64
	// watchers 是**观察者 goroutine 计数**（每次 ReserveUsage 至多 1 个；m7：此前每轮等待
	// 都 spawn 一个 → 高竞争下 goroutine churn + 野 Broadcast）。测试断言其有界。
	watchers atomic.Int64
	// beforeWait 是**测试钩子**（生产恒 nil）：在「确认不足、即将进入等待」的竞态窗口点
	// 调用（**不持 cond.L**，仅用于把「释放恰在 TryReserve 失败与 Wait 之间发生」确定化）。
	beforeWait func()
}

// NewStagingTracker 构造 staging 配额记账器（scope 为独立 staging Scope）。
func NewStagingTracker(scope *Scope) *StagingTracker {
	return &StagingTracker{scope: scope, cond: sync.NewCond(&sync.Mutex{}), waitTimeout: stagingWaitTimeout}
}

// ReserveUsage 预留 size 字节（本地 staging 写入前）。不足 → 排队等待（ctx 取消 /
// 等待超时中断）；返回错误 = 拒绝本次写入（永久不足快速失败）。
//
// 超时判据基于**绝对 deadline**（非 timer.Stop 结果）——避免「空间刚好释放但 timeout
// 定时器已触发」的边界误判：每次唤醒先重试 TryReserve，仅当已过 deadline 且抢占
// 仍失败才报超时。
func (q *StagingTracker) ReserveUsage(ctx context.Context, size int64) error {
	if size <= 0 {
		return nil
	}
	timeout := q.waitTimeout
	if timeout <= 0 {
		timeout = stagingWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	// 观察者**懒起且至多 1 个**（见 waitWatcher）：快路径零 goroutine；仅在真正进入等待时
	// 创建，供 ctx 取消/超时 `Broadcast` 打断 `cond.Wait`（m7：此前**每轮等待**各 spawn 一个
	// → 高竞争下 goroutine churn + 野 Broadcast）。
	w := &waitWatcher{}
	defer w.stop()
	for {
		if res, err := q.scope.TryReserve(size); err == nil {
			res.Commit(size)
			return nil
		}
		// 不足：进入一轮排队等待（含持锁 double-check / ctx / deadline 判定）。
		done, err := q.waitRound(ctx, size, deadline, timeout, timer, w)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// waitRound 执行「不足 → 排队等待」的一轮：**持 cond.L** 先 double-check（闭死 TryReserve
// 失败与 Wait 之间的释放窗口，评审 F4），再判 ctx/deadline，最后 arm + Wait。
// 返回 done=true 表示本轮已成功预留；err != nil 表示应终止（ctx 取消 / 排队超时）。
func (q *StagingTracker) waitRound(ctx context.Context, size int64, deadline time.Time,
	timeout time.Duration, timer *time.Timer, w *waitWatcher) (bool, error) {
	if q.beforeWait != nil {
		q.beforeWait() // 测试钩子：窗口点（不持锁）
	}
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	if res, rerr := q.scope.TryReserve(size); rerr == nil {
		res.Commit(size)
		return true, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return false, fmt.Errorf("quota: 等待本地 staging 空间中断: %w", cerr)
	}
	if time.Until(deadline) <= 0 {
		// deadline 后最后一次尝试：释放可能与超时同时发生，避免误报。
		if res, rerr := q.scope.TryReserve(size); rerr == nil {
			res.Commit(size)
			return true, nil
		}
		return false, fmt.Errorf("quota: 等待本地 staging 空间超时（%s）", timeout)
	}
	w.arm(q, ctx, timer)
	q.waitRounds.Add(1) // 进入等待（测试据此同步，无需固定 Sleep）
	q.cond.Wait()       // 释放 cond.L；被唤醒后 defer 重新解锁
	return false, nil
}

// waitWatcher 承载一次 ReserveUsage 的等待观察者（懒起一次；stop 幂等）。
// 抽取原因：让 ReserveUsage 保持可读（gocognit 阈值）且观察者语义单点。
type waitWatcher struct {
	interrupt chan struct{}
	armed     bool
}

// arm 首次进入等待时启动观察者：ctx 取消 / 超时 → Broadcast 打断所有等待者重新评估。
// **调用方持 cond.L**（与 Wait 同一临界区）——观察者取锁后才 Broadcast，故信号不会落在
// 「尚未 Wait」的窗口里（F4 的另一半：唤醒不可丢）。
func (w *waitWatcher) arm(q *StagingTracker, ctx context.Context, timer *time.Timer) {
	if w.armed {
		return
	}
	w.armed = true
	w.interrupt = make(chan struct{})
	q.watchers.Add(1)
	go func() {
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-w.interrupt:
			return
		}
		q.cond.L.Lock() // 等待者持锁直至 Wait 释放 ⇒ 此锁必在 Wait 之后取得
		q.cond.Broadcast()
		q.cond.L.Unlock()
	}()
}

// stop 停止观察者（未 arm 时 no-op；重复调用安全由调用方一次性 defer 保证）。
func (w *waitWatcher) stop() {
	if w.interrupt != nil {
		close(w.interrupt)
	}
}

// ReleaseUsage 释放 size 字节（上传成功或失败后调用；唤醒排队等待者）。
// **持 cond.L 释放+广播**（评审 F4）：否则广播可能落在等待者「TryReserve 失败 → Wait」
// 的窗口里被永久丢失。锁序：cond.L → scope.mu（与 ReserveUsage 同序，无环）。
func (q *StagingTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.cond.L.Lock()
	q.scope.ReleaseUsage(size)
	q.cond.Broadcast() // 唤醒排队等待者
	q.cond.L.Unlock()
}
