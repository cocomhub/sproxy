// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"errors"
	"fmt"
	"sync"
)

// allocation.go 实现 **配额分配账本**（配额模型 v3，2026-10-14 用户裁定）。
//
// 结构：**卷（物理上限）→ 用户（每卷独立配额）→ 目录（目录级最大配额）**。
// 不变式：**Σ 子节点上限 ≤ 父节点上限**（父上限 0 = 不限 ⇒ 不做约束）。
// 「未分配额度」= 父上限 − Σ子上限 ⇒ 直接支撑「每卷总/已分配/剩余」的可见性。
//
// **与用量严格分离**：分配是**策略额度**（允许谁用多少），用量是**实际占用**（committed/reserved）。
// 二者混算会让「已分配但未使用」的空间被误判为已用（假满）；反之会让超额分配（卷被打满）。
//
// 用法（配置路径唯一入口）：`pool.EnsureChild(segs, maxBytes)` —— 创建/改额并同步分配账本；
// 内部解析（只读用量）继续用 `ResolvePath/EnsureScope`（不涉及分配）。

// Allocation 是一次分配额度的句柄（可改额/归还）。
type Allocation struct {
	mu       sync.Mutex
	pool     *Pool
	amount   int64
	released bool
}

// TryAllocate 从本节点分配 n 字节额度（父上限非 0 且超额 ⇒ 错误，额度不变）。
func (p *Pool) TryAllocate(n int64) (*Allocation, error) {
	if p == nil {
		return nil, errors.New("quota: nil pool")
	}
	n = nonNeg(n)
	if err := p.addAllocated(n); err != nil {
		return nil, err
	}
	return &Allocation{pool: p, amount: n}, nil
}

// addAllocated 原子地增加已分配额度（校验不变式）。
func (p *Pool) addAllocated(n int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.maxBytes > 0 && p.allocated+n > p.usableLocked() {
		return fmt.Errorf("quota: 分配超限（已分配 %d + 本次 %d > 可用上限 %d）", p.allocated, n, p.usableLocked())
	}
	p.allocated += n
	p.broadcastLocked()
	return nil
}

// subAllocated 归还额度（防御性钳 0：额度调整与释放可能交错）。
func (p *Pool) subAllocated(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allocated -= n
	if p.allocated < 0 {
		p.allocated = 0
	}
	p.broadcastLocked()
}

// Release 归还本次分配（幂等）。
func (a *Allocation) Release() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return
	}
	a.released = true
	a.pool.subAllocated(a.amount)
}

// Set 调整本次分配额度为 n（差额分配/归还）；超额 ⇒ 错误且额度不变（原子）。
func (a *Allocation) Set(n int64) error {
	if a == nil {
		return errors.New("quota: nil allocation")
	}
	n = nonNeg(n)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return errors.New("quota: allocation 已归还")
	}
	delta := n - a.amount
	if delta > 0 {
		if err := a.pool.addAllocated(delta); err != nil {
			return err
		}
	} else if delta < 0 {
		a.pool.subAllocated(-delta)
	}
	a.amount = n
	return nil
}

// Amount 返回当前分配额度。
func (a *Allocation) Amount() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.amount
}

// Allocated 返回本节点已分配给子节点的额度（Σ子上限）。
func (p *Pool) Allocated() int64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.allocated
}

// Unallocated 返回本节点**尚未分配**的额度（上限 − 已分配；上限 0 = 不限 ⇒ 返回 0 表示不适用）。
func (p *Pool) Unallocated() int64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.maxBytes <= 0 {
		return 0
	}
	left := max(p.usableLocked()-p.allocated, 0)
	return left
}

// EnsureChild 取得/创建 segs 对应的子节点并把其**上限**设为 maxBytes，同时同步父节点的分配账本
// （差额分配/归还）——**配置路径（用户配额/目录配额）的唯一入口**：Σ子上限 ≤ 父上限由本方法保证。
//
// 中间层节点（未给上限）以 0（不限）创建、不占用分配；只对**末节点**记分配。
// 超额 ⇒ 返回错误且**不改动任何状态**（父分配与子上限都不变）。
func (p *Pool) EnsureChild(segs []string, maxBytes int64) (*Scope, error) {
	if p == nil {
		return nil, errors.New("quota: nil pool")
	}
	maxBytes = nonNeg(maxBytes)
	// 定位/创建末节点（中间层 max 0）。
	cur := p
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			child = cur.newScope(seg, 0)
		}
		cur = child.pool
		_ = i // 中间层仅作路径承载
	}
	if cur == p && len(segs) == 0 {
		return nil, errors.New("quota: EnsureChild 需要非空路径")
	}
	// 同步分配（末节点上限从旧值调整为新值）。
	cur.mu.RLock()
	old := cur.maxBytes
	cur.mu.RUnlock()
	delta := maxBytes - old
	if delta > 0 {
		if err := p.addAllocated(delta); err != nil {
			return nil, err
		}
	} else if delta < 0 {
		p.subAllocated(-delta)
	}
	cur.mu.Lock()
	cur.maxBytes = maxBytes
	cur.mu.Unlock()
	return &Scope{pool: cur}, nil
}

// EnsureChild 是 Scope 侧的同一入口（在本节点之下创建/改额子节点并同步分配账本）。
// 语义与 Pool.EnsureChild 完全一致——Scope 即「本节点」的视图（s.pool 就是该节点）。
func (s *Scope) EnsureChild(segs []string, maxBytes int64) (*Scope, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("quota: nil scope")
	}
	return s.pool.EnsureChild(segs, maxBytes)
}

// Allocated / Unallocated 的 Scope 侧视图（同 Pool）。
func (s *Scope) Allocated() int64 {
	if s == nil || s.pool == nil {
		return 0
	}
	return s.pool.Allocated()
}

func (s *Scope) Unallocated() int64 {
	if s == nil || s.pool == nil {
		return 0
	}
	return s.pool.Unallocated()
}
