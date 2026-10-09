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
}

// NewStagingTracker 构造 staging 配额记账器（scope 为独立 staging Scope）。
func NewStagingTracker(scope *Scope) *StagingTracker {
	return &StagingTracker{scope: scope, cond: sync.NewCond(&sync.Mutex{})}
}

// ReserveUsage 预留 size 字节（本地 staging 写入前）。不足 → 排队等待（ctx 取消 /
// 等待超时中断）；返回错误 = 拒绝本次写入（永久不足快速失败）。
func (q *StagingTracker) ReserveUsage(ctx context.Context, size int64) error {
	if size <= 0 {
		return nil
	}
	timer := time.NewTimer(stagingWaitTimeout)
	defer timer.Stop()
	for {
		res, err := q.scope.TryReserve(size)
		if err == nil {
			res.Commit(size)
			return nil
		}
		// 磁盘不足：排队等待释放（ctx 取消 / 超时中断；cond 无 ctx 原语——用 goroutine
		// 感知 ctx/timer 并 Broadcast 打断 cond.Wait）。
		q.cond.L.Lock()
		interrupt := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				q.cond.Broadcast() // 打断所有等待者重新评估（含本 ctx 取消者）
			case <-timer.C:
				q.cond.Broadcast() // 等待超时：打断重新评估（本循环感知后返回错误）
			case <-interrupt:
			}
		}()
		q.cond.Wait()
		close(interrupt)
		q.cond.L.Unlock()
		if ctx.Err() != nil {
			return fmt.Errorf("quota: 等待本地 staging 空间中断: %w", ctx.Err())
		}
		if !timer.Stop() {
			return fmt.Errorf("quota: 等待本地 staging 空间超时（%s）", stagingWaitTimeout)
		}
	}
}

// ReleaseUsage 释放 size 字节（上传成功或失败后调用；唤醒排队等待者）。
func (q *StagingTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.scope.ReleaseUsage(size)
	q.cond.Broadcast() // 唤醒排队等待者
}
