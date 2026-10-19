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
// 不变式 I1：**Σ **直接子**节点上限 ≤ 父节点可用上限**（父上限 0 = 不限 ⇒ 不做约束）——
// **carve-out（额度再切分）模型**：子额度是父额度的再切分 ⇒ 只记直接父，不逐级上溯
// （逐级会双计：alice(600) 与 alice/user/videos(300) 会把祖父记成 900）。
// 「未分配额度」= 父上限 − Σ子上限 ⇒ 直接支撑「每卷总/已分配/剩余」的可见性。
//
// **与用量严格分离**：分配是**策略额度**（允许谁用多少），用量是**实际占用**（committed/reserved）。
// 二者混算会让「已分配但未使用」的空间被误判为已用（假满）；反之会让超额分配（卷被打满）。
//
// 用法（配置路径唯一入口）：`pool.EnsureChild(segs, maxBytes)` —— 创建/改额并同步分配账本；
// 内部解析（只读用量）继续用 `ResolvePath/EnsureScope`（不涉及分配）。

// 注：`Pool.allocMu`（quota.go）是跨层分配账本更新的唯一临界区（见 EnsureChild）。

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
	// 归还额度 ⇒ 在根等待锁下变更（消除丢唤醒；锁序 a.mu → waitMu → node.mu）。
	a.pool.withWaitLock(func() { a.pool.subAllocated(a.amount) })
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
		// 缩额释放了额度 ⇒ 在根等待锁下变更（消除丢唤醒）。
		a.pool.withWaitLock(func() { a.pool.subAllocated(-delta) })
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
// （差额分配/归还）——**分配账本（I1）的唯一入口**：Σ子树上限 ≤ 父可用上限由本方法保证。
//
// **现状（独立复审 B-P2-4，2026-10-18 如实标注）**：生产装配路径尚未接线到这里——服务端建配额树
// 仍走 `Pool.Scope`/`Scope.Mount`/`EnsureScope`（pkg/server/handlers.go 的 quotaScopeFor），
// 这些只建节点**不记分配账** ⇒ 生产环境 `Allocated()/Unallocated()` 目前为 0/失真、I1 在这些
// 节点上**尚无强制点**（用量侧 I2 不受影响：仍由 reserveUp 沿父链强制）。接线计划见设计 §S4。
//
// 中间层节点（未给上限）以 0（不限）创建、不占用分配；只对**末节点**记分配。
// 超额 ⇒ 返回错误且**不改动任何状态**（父分配与子上限都不变）。
func (p *Pool) EnsureChild(segs []string, maxBytes int64) (*Scope, error) {
	if p == nil {
		return nil, errors.New("quota: nil pool")
	}
	// 变更分配账本会改变可用额度 ⇒ 走**根等待锁**（锁序 waitMu → allocMu → node.mu）⇒
	// 与等待侧的 double-check/Wait 互斥（消除丢唤醒）。
	var (
		sc  *Scope
		err error
	)
	p.withWaitLock(func() { sc, err = p.ensureChildLocked(segs, maxBytes) })
	return sc, err
}

// setChildLimit 把 leaf 的上限设为 maxBytes 并**同步分配账本**——**唯一记账实现**（I1 强制点）。
// 调用方须持全局根 waitMu（本函数内部取全局根 allocMu）；超额 ⇒ **原子失败**（不改动任何状态，
// 含回滚本次新建节点）。delta==0 为空操作。
//
// **为何抽出来**：生产装配路径（server 的 ensureTenantQuotaLocked）用 `Pool.Scope`/`Scope.Mount`/
// `EnsureScope` 建树，其**路径语义**与 `EnsureChild` 的逐段拼接不同（joinPath 保留前导 "/"）⇒
// 不能直接委托；但**记账必须同一实现**（独立复审 B-P2-4：否则又是"多套同时存在"，且生产环境
// Allocated()/Unallocated() 恒 0/失真、I1 无强制点）。故三者与本函数共用同一记账。
func (p *Pool) setChildLimit(leaf *Pool, maxBytes int64, created []*Pool) error {
	// 串行化用**全局根**的 allocMu（覆盖读旧值/记账/写新上限）。锁序：waitMu → allocMu → node.mu。
	p.rootPool().allocMu.Lock()
	defer p.rootPool().allocMu.Unlock()
	leaf.mu.RLock()
	old := leaf.maxBytes
	leaf.mu.RUnlock()
	delta := maxBytes - old
	if delta == 0 {
		return nil
	}
	if aerr := p.adjustParentAllocationLocked(leaf, delta); aerr != nil {
		// 记账归属＝**直接父**（carve-out 模型，见 adjustParentAllocationLocked）；失败 ⇒ 摘除新建节点
		// （独立复审 P3：此前新建节点留在树里 ⇒ 树结构已变，与「超额 ⇒ 不改动任何状态」矛盾）。
		for _, n := range created {
			n.detachLocked()
		}
		return aerr
	}
	leaf.mu.Lock()
	leaf.maxBytes = maxBytes
	leaf.mu.Unlock()
	return nil
}

// ensureChildLocked 是 EnsureChild 在**根等待锁内**的实现（语义见 EnsureChild 注释）。
func (p *Pool) ensureChildLocked(segs []string, maxBytes int64) (*Scope, error) {
	cur, created, err := p.ensureChildTarget(segs)
	if err != nil {
		return nil, err
	}
	if lerr := p.setChildLimit(cur, nonNeg(maxBytes), created); lerr != nil {
		return nil, lerr
	}
	return &Scope{pool: cur}, nil
}

// ensureChildTarget 沿 segs 定位末节点（中间层以 max=0 创建），返回末节点与**本次新建的节点**
// （供记账失败时回滚）。全空段（如 [""]）必须拒绝（独立复审 P2）：此前只判 len(segs)==0 ⇒ 这类
// 输入返回 cur==p，随后记账会沿祖先一路走到**全局根**（正是自陈的「双计」形态）。
func (p *Pool) ensureChildTarget(segs []string) (*Pool, []*Pool, error) {
	cur := p
	descended := false
	var created []*Pool
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			child = cur.newScope(seg, 0)
			created = append(created, child.pool)
		}
		cur = child.pool
		descended = true
	}
	if !descended {
		return nil, nil, errors.New("quota: EnsureChild 需要非空路径段")
	}
	return cur, created, nil
}

// adjustParentAllocationLocked 把 leaf 的上限变化 delta 记到**它的直接父**。
//
// **语义＝carve-out（额度再切分）模型**（权威规格 I1 的落地，2026-10-18 定稿）：每个节点的
// `allocated` = **Σ 其直接子节点的上限**（即"切出去给子节点的额度"）。子节点的额度是**父节点额度的
// 再切分** ⇒ 只记直接父；**记到祖父会双计**（实测：`alice(600)` + `alice/user/videos(300)` 会把
// 祖父记成 600+300=900 ⇒ 同卷第二个租户 `bob(400)` 被误拒 ✗）。
// 「目录配额 ≤ 用户配额 ≤ 卷上限」的**实际约束**由**用量**沿父链强制（`reserveUp`：committed+
// reserved+n ≤ 每层可用上限）——分配账本只负责「切分额度不超额」这一层。
//
// 形态无关性（独立复审 P1）：记账只依赖 leaf.parent ⇒ `p.EnsureChild(["a","b"],1)` 与
// `a.EnsureChild(["b"],1)` 结果**必然一致**。
// 调用方须持全局根 allocMu；delta<0 为归还（不会失败）。
func (p *Pool) adjustParentAllocationLocked(leaf *Pool, delta int64) error {
	parent := leaf.parent
	if parent == nil {
		return nil // 叶即根（ensureChildTarget/ensureScopeWithLimit 已拒绝全空段）
	}
	if delta < 0 {
		parent.subAllocated(-delta)
		return nil
	}
	return parent.addAllocated(delta)
}

// detachLocked 从父节点的 children 中摘除自身（EnsureChild 记账失败时回滚本次新建的节点；
// 调用方须持全局根 allocMu）。锁序同 EnsureChild（allocMu → node.mu）。
func (p *Pool) detachLocked() {
	parent := p.parent
	if parent == nil {
		return
	}
	parent.mu.Lock()
	defer parent.mu.Unlock()
	kept := parent.children[:0]
	for _, c := range parent.children {
		if c.pool != p {
			kept = append(kept, c)
		}
	}
	parent.children = kept
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
