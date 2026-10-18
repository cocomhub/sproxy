// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// wait.go 实现**配额不足时的排队等待重试**（2026-10-14 用户裁定）：
// 「严格上界预占 + 不够时继续队列重试等待」——这样**真正放得下的文件最终一定成功**，而不是被一次 507
// 直接拒掉；同时因为预占是严格上界，「绝不打满磁盘/绝不超真实配额」仍由预留保证。
//
// 形态与既有 staging 排队（`staging_tracker.go`）一致：**释放广播唤醒 + 持锁 double-check（闭死丢唤醒
// 窗口）+ ctx 取消 / 超时中断**；区别是等待挂在**根节点**的队列上（任何一层释放都可能让等待者满足）。
//
// 超时上限（用户裁定）：默认 `DefaultWaitTimeout`（30s）；超时后返回**包装 ErrStorageFull** 的错误
// （调用方映射 507 + 提示「释放空间或申请提额」）。

// WaitRounds 返回本节点进入等待的轮次（可观测性/测试同步用）。
func (p *Pool) WaitRounds() int64 {
	if p == nil {
		return 0
	}
	return p.waitRounds.Load()
}

// DefaultWaitTimeout 是配额等待的默认上限（用户裁定 30s）。
const DefaultWaitTimeout = 30 * time.Second

// ErrWaitTimeout 表示等待配额释放超时（包装 ErrStorageFull，调用方仍按容量不足处理）。
var ErrWaitTimeout = errors.New("quota: 等待配额释放超时")

// WaitReserve 尝试预留 n 字节；容量不足时**排队等待**（释放广播 / ctx 取消 / 超时），超时或取消才失败。
// timeout<=0 ⇒ DefaultWaitTimeout。
//
// 语义要点：
//   - **严格上界**由调用方保证（本函数只负责排队重试）；
//   - 非容量类错误（非法参数等）**立即返回**，不排队；
//   - 成功返回的 Reservation 与 TryReserve 完全一致（Commit/Release 语义相同）。
func (s *Scope) WaitReserve(ctx context.Context, n int64, timeout time.Duration) (*Reservation, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("quota: nil scope")
	}
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		res, err := s.TryReserve(n)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, ErrStorageFull) {
			return nil, err
		}
		// **不可能容纳 ⇒ 立即失败，不排队**（严格上界判据）：若 n 超过某一层的
		// `maxBytes − 运维余量`（即使该层 committed/reserved 全归零也放不下）⇒ 等待**永远不会
		// 成功** ⇒ 排队只会白等满超时。
		// 实测（CI `Test (ubuntu, +Vault)` 超时 5min）：R31 把 WaitReserve 接入上传主路径后，
		// 全仓大量「请求 > 上限」的用例各自白等 30s（DefaultWaitTimeout）累计触发测试超时；
		// 生产表现为「确定的 507 也要先挂 30s」（可用性回归）。
		if s.pool.canNeverFit(n) {
			return nil, fmt.Errorf("%w: 请求 %d 字节超过容量上限（即使全部释放也无法容纳，不排队）",
				ErrStorageFull, n)
		}
		if werr := s.pool.waitRound(ctx, n, deadline); werr != nil {
			return nil, werr
		}
	}
}

// WaitReserve 是 Pool 侧的同一入口（在本节点上预留并排队等待）。
func (p *Pool) WaitReserve(ctx context.Context, n int64, timeout time.Duration) (*Reservation, error) {
	if p == nil {
		return nil, errors.New("quota: nil pool")
	}
	return (&Scope{pool: p}).WaitReserve(ctx, n, timeout)
}

// canNeverFit 判断 n 是否**在任何释放下都无法容纳**：只要有一层满足 `n > maxBytes − 运维余量`
// （即该层全部容量都给它也不够）⇒ 永远等不到 ⇒ 调用方必须立即失败（见 WaitReserve）。
// 无上限层（maxBytes<=0）不构成约束。
func (p *Pool) canNeverFit(n int64) bool {
	for cur := p; cur != nil; cur = cur.parent {
		cur.mu.RLock()
		limit := cur.usableLocked()
		cur.mu.RUnlock()
		if limit > 0 && n > limit {
			return true
		}
	}
	return false
}

// waitRound 执行一轮「不足 → 排队等待」：**持根节点锁**先 double-check（释放可能刚刚发生 ⇒ 立即返回
// 让外层重试，避免丢唤醒），否则 Wait 直到被释放广播 / ctx 取消 / 超时打断。
func (p *Pool) waitRound(ctx context.Context, n int64, deadline time.Time) error {
	root := p
	for root.parent != nil {
		root = root.parent
	}
	if root.cond == nil {
		return fmt.Errorf("quota: 等待队列未初始化: %w", ErrStorageFull)
	}
	// **打断回调必须持锁广播**（与 staging_tracker 同型）：否则广播可能落在「已判定不足 → 尚未
	// Wait」的窗口内 ⇒ 丢唤醒（等待者会睡到超时）。持锁后，回调只能在我们 Wait（释放锁）之后完成
	// 广播 ⇒ 必被唤醒。
	broadcastLocked := func() {
		root.cond.L.Lock()
		root.cond.Broadcast()
		root.cond.L.Unlock()
	}
	root.cond.L.Lock()
	if err := ctx.Err(); err != nil {
		root.cond.L.Unlock()
		return err
	}
	if !time.Now().Before(deadline) {
		root.cond.L.Unlock()
		return fmt.Errorf("%w: %w", ErrWaitTimeout, ErrStorageFull)
	}
	// double-check：**沿父链**确认是否已有容量 ⇒ 直接让外层重试（不睡）。
	// 只看本节点是错的：失败点常在父节点（卷满/用户配额满），父节点释放后本节点仍「有容量」⇒
	// 会忙等空转（既不等待也不返回）。锁序恒为 子→父（与 reserveUp 一致，无反向嵌套 ⇒ 无死锁）。
	ok := true
	for cur := p; cur != nil; cur = cur.parent {
		cur.mu.RLock()
		avail := cur.maxBytes <= 0 || cur.committed+cur.reserved+n <= cur.usableLocked()
		cur.mu.RUnlock()
		if !avail {
			ok = false
			break
		}
	}
	if ok {
		root.cond.L.Unlock()
		return nil
	}
	// 超时 / ctx 取消打断 Wait：AfterFunc 无常驻 goroutine（与 staging 的 watcher 等价、更轻）。
	timer := time.AfterFunc(time.Until(deadline), broadcastLocked)
	stopCtx := context.AfterFunc(ctx, broadcastLocked)
	// Wait 前**重检** ctx/deadline（回调可能已在此前触发；重检闭死「已取消仍睡」）。
	if err := ctx.Err(); err != nil {
		timer.Stop()
		stopCtx()
		root.cond.L.Unlock()
		return err
	}
	if !time.Now().Before(deadline) {
		timer.Stop()
		stopCtx()
		root.cond.L.Unlock()
		return fmt.Errorf("%w: %w", ErrWaitTimeout, ErrStorageFull)
	}
	root.waitRounds.Add(1) // 进入等待（测试据此确定性同步；见 Pool.waitRounds）
	root.cond.Wait()
	root.cond.L.Unlock()
	// **解锁后**再 stop：回调可能正阻塞在 cond.L 上，若在持锁时 stop 会与回调互等（死锁）。
	timer.Stop()
	stopCtx()
	return nil
}
