// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// waitq_adapter.go 是配额池接入 `pkg/waitq` 通用等待队列的**适配器**（架构级收敛，
// 设计 docs/designs/2026-10-19-waitq.md §8）：把「配额不足时排队重试」从 pkg/quota 私有的
// `sync.Cond` 实现收敛到通用能力包，同时**不丢失**任何能力（严格上界 / 沿父链强制 /
// 不可能容纳立即失败 / 超时与取消 / 进入等待的可观测计数）。
//
// 丢唤醒（设计 §3.1 I1）的正确闭合：等待侧「查 + 入队」与释放侧「改状态 + 唤醒」都在
// **根等待锁**（`Pool.waitMu`）内 ⇒ 释放要么发生在等待者临界区之前（则其快速尝试必然成功、
// 不入队），要么之后（则等待者已在队列中，唤醒必达）。见 waitq.AcquireUnder。

import (
	"errors"
	"time"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// poolResource 是「某节点为根的子配额树」的 waitq.Resource 适配器。
//
// 语义与既有 WaitReserve 完全一致：
//   - Capacity/Used 取**父链上最紧的一层**（任一层不足即整体不足）；
//   - TryAcquire 即该 scope 的 TryReserve（沿父链预留，严格上界）；
//   - Release 只在 waitq 的**回滚路径**（多资源失败回滚）或调用方显式放弃时生效 —— 见 take。
type poolResource struct {
	p *Pool
	// holder 是本次请求的唯一键：TryAcquire 成功后把 Reservation 暂存于此，供 WaitReserve
	// 取回（waitq 的 Lease 不承载配额语义 ⇒ 由调用方接管 Reservation）。
	holder string
}

// Name 实现 waitq.Resource（资源标识：根池路径 + 节点路径）。
func (r poolResource) Name() string {
	return "quota:" + r.p.waitqPath()
}

// Capacity 实现 waitq.Resource：父链上最紧一层的**可用上限**（maxBytes − 运维余量）；
// 全部无上限时返回 0（waitq 视为无上限）。
func (r poolResource) Capacity() int64 {
	limit, _ := r.p.waitqLimits()
	return limit
}

// Used 实现 waitq.Resource：`Capacity() - 最紧层剩余可用` ⇒ `Capacity()-Used()` 即剩余可用，
// 供 waitq 判断「唤醒后是否可能满足」（I2）。
func (r poolResource) Used() int64 {
	limit, free := r.p.waitqLimits()
	return limit - free
}

// TryAcquire 实现 waitq.Resource：调用既有 TryReserve（沿父链严格上界预留），成功则暂存
// Reservation 供取回；不足返回 waitq.ErrInsufficient（waitq 据此排队）。
func (r poolResource) TryAcquire(n int64) error {
	res, err := r.p.reserveForHolder(r.holder, n)
	if err != nil {
		if errors.Is(err, ErrStorageFull) {
			return waitq.ErrInsufficient
		}
		return err
	}
	_ = res
	return nil
}

// Release 实现 waitq.Resource：**仅回滚/放弃路径**生效 —— 若本次 holder 的 Reservation 仍在
// 暂存区（调用方尚未接管）则释放它；已被取回（调用方持有）则 no-op（**绝不重复释放**）。
func (r poolResource) Release(int64) {
	r.p.dropHolder(r.holder, true)
}

// take 取回本次 holder 的 Reservation（调用方接管配额所有权；取回后 Release 变 no-op）。
func (r poolResource) take() *Reservation {
	return r.p.dropHolder(r.holder, false)
}

// waitqPath 返回该节点的资源标识路径（根池为 "root"）。
func (p *Pool) waitqPath() string {
	if p.path == "" {
		return "root"
	}
	return p.path
}

// waitqLimits 返回 (父链上最紧的可用上限, 该层剩余可用)：
//   - limit = min{ usableLocked() }（忽略无上限层 maxBytes<=0）；
//   - free  = min{ usableLocked() - committed - reserved }（同一层的剩余）。
//
// 全部无上限 ⇒ (0, 0)（waitq 视为无上限、恒可满足）。锁序：node.mu（自内向外逐层取放，
// 不嵌套持有两把 node.mu ⇒ 无死锁）。
func (p *Pool) waitqLimits() (limit, free int64) {
	limit, free = 0, 0
	first := true
	for cur := p; cur != nil; cur = cur.parent {
		cur.mu.RLock()
		lim := cur.usableLocked()
		avail := lim - cur.committed - cur.reserved
		cur.mu.RUnlock()
		if lim <= 0 {
			continue // 该层无上限：不构成约束
		}
		if first || lim < limit {
			limit = lim
		}
		if first || avail < free {
			free = avail
		}
		first = false
	}
	if first {
		return 0, 0
	}
	if free < 0 {
		free = 0 // 已超限（对账窗口）⇒ 视为无剩余，避免负数参与判据
	}
	return limit, free
}

// reserveForHolder 执行一次预留并把 Reservation 暂存到 holder（供 WaitReserve 取回）。
func (p *Pool) reserveForHolder(holder string, n int64) (*Reservation, error) {
	res, err := (&Scope{pool: p}).TryReserve(n)
	if err != nil {
		return nil, err
	}
	p.holderMu.Lock()
	if p.holders == nil {
		p.holders = make(map[string]*Reservation, 1)
	}
	p.holders[holder] = res
	p.holderMu.Unlock()
	return res, nil
}

// dropHolder 移除 holder 的暂存 Reservation；release=true 时同时释放它（回滚/放弃）。
// 返回被移除的 Reservation（未暂存时返回 nil ⇒ release 也是 no-op）。
func (p *Pool) dropHolder(holder string, release bool) *Reservation {
	p.holderMu.Lock()
	res := p.holders[holder]
	delete(p.holders, holder)
	p.holderMu.Unlock()
	if res != nil && release {
		res.Release()
	}
	return res
}

// waitqMetrics 把 waitq 的等待者变化接到本池的 waitRounds 计数（保留既有可观测契约：
// 「进入等待的轮次」= 入队次数；测试据此确定性同步，无需 time.Sleep）。
type waitqMetrics struct{ root *Pool }

// IncWaiters 实现 waitq.Metrics：入队（delta=+1）即一轮等待。
func (m waitqMetrics) IncWaiters(delta int) {
	if delta > 0 {
		m.root.waitRounds.Add(int64(delta))
	}
}

// ObserveWait 实现 waitq.Metrics（当前无指标后端接入；保留扩展点）。
func (m waitqMetrics) ObserveWait(_ time.Duration, _ string) {}
