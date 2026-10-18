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
	// 串行化用**全局根**的 allocMu，且必须**覆盖节点定位与创建**（独立复审 P2，2026-10-18）：
	// 定位内含 childWithSegment(RLock) + newScope(锁写)；若置于锁外，并发同段调用会各自创建
	// **重复兄弟节点**并重复记账（幽灵节点 + 父 allocated 记重 ⇒ 后续合法分配假失败）。
	// 锁序：allocMu → node.mu（本函数与调用链内不得反向获取）。
	root := p
	for root.parent != nil {
		root = root.parent
	}
	root.allocMu.Lock()
	defer root.allocMu.Unlock()
	cur, created, err := p.ensureChildTarget(segs)
	if err != nil {
		return nil, err
	}
	cur.mu.RLock()
	old := cur.maxBytes
	cur.mu.RUnlock()
	if delta := maxBytes - old; delta != 0 {
		// 记账归属＝**直接父**（见 adjustParentAllocationLocked）；失败 ⇒ 摘除本次新建的中间层
		// 节点（独立复审 P3：此前新建节点留在树里 ⇒ 树结构已变、可被 Resolve 命中，与
		// 「超额 ⇒ 不改动任何状态」承诺矛盾）。
		if aerr := p.adjustSubtreeAllocationsLocked(cur, delta); aerr != nil {
			for _, n := range created {
				n.detachLocked()
			}
			return nil, aerr
		}
		cur.mu.Lock()
		cur.maxBytes = maxBytes
		cur.mu.Unlock()
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

// adjustSubtreeAllocationsLocked 把 leaf 的上限变化 delta 记到**它的每个祖先直到全局根**。
//
// **语义（权威规格 I1 的落地）**：每个节点的 `allocated` = 该节点**子树内所有节点上限之和**
// ——即"这个子树占用了父多少额度"。故 leaf 上限变化必须同时反映到**每一级祖先**（含根）。
// 这样「目录配额 ≤ 用户配额 ≤ 卷上限」在任何深度都成立（既有用例
// `model_v3_probe_test.go:127-131` 钉住的正是这一点）。
//
// **必须走到全局根，不能只走到调用目标 `p`**（独立复审 P1，2026-10-18 实测反例）：只走到 p 会让
// 同一逻辑树**因调用形态不同结果不同**（`p.EnsureChild(["a"],1000)` 后 `p.EnsureChild(["a","b"],1)`
// 被拒，而 `a.EnsureChild(["b"],1)` 同树通过）——这是**形态相关**缺陷（结构不变式被调用方式影响）。
// 走到全局根后两种形态完全等价（两者都从 leaf.parent 上溯到同一根）。
// 任一级拒绝 ⇒ 已记账者**逐级回滚**（保持「超额 ⇒ 不改动任何状态」承诺）。
// 调用方须持全局根 allocMu；delta<0 为归还（不会失败）。
func (p *Pool) adjustSubtreeAllocationsLocked(leaf *Pool, delta int64) error {
	if delta < 0 {
		for anc := leaf.parent; anc != nil; anc = anc.parent {
			anc.subAllocated(-delta)
		}
		return nil
	}
	var done []*Pool
	for anc := leaf.parent; anc != nil; anc = anc.parent {
		if err := anc.addAllocated(delta); err != nil {
			for _, r := range done {
				r.subAllocated(delta)
			}
			return err
		}
		done = append(done, anc)
	}
	return nil
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
