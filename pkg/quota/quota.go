// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package quota 提供通用配额池（Pool/Scope/Reservation）。
// 子作用域通过路径化叠加挂载，子池操作自动向父链聚合；预留通过 Reservation 句柄对账。
// 本包不引入租户/存储概念，是通用的配额领域模型。
package quota

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrStorageFull 表示配额超限（子作用域上限或全局兜底上限）。
var ErrStorageFull = errors.New("storage quota exceeded")

// Pool 是配额账本节点（根池，或某个 Scope 的底层账本）。
// 账本记录已确认占用 committed 与预留中 reserved；可用额度 = maxBytes − (committed + reserved)。
type Pool struct {
	mu        sync.RWMutex
	maxBytes  int64 // 0 = 不限制
	committed int64
	reserved  int64
	parent    *Pool // 非 nil 时向父链聚合
	children  []*Scope
	path      string // 根池为 ""；子池为对应 Scope 的完整路径
}

// NewPool 创建根配额池。maxBytes<=0 表示不限制。
func NewPool(maxBytes int64) *Pool {
	return &Pool{maxBytes: maxBytes}
}

// Scope 返回挂载到根池上的新子作用域（路径化叠加，maxBytes<=0 不限制）。
func (p *Pool) Scope(path string, maxBytes int64) *Scope {
	return p.newScope(path, maxBytes)
}

// Usage 返回本池聚合占用（含子孙作用域传播）。
func (p *Pool) Usage() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.committed
}

// Reserved 返回本池聚合预留。
func (p *Pool) Reserved() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reserved
}

// MaxBytes 返回本池上限（0 = 不限制）。
func (p *Pool) MaxBytes() int64 {
	return p.maxBytes
}

// SetMaxBytes 运行时调整本池上限（0 = 不限制）。仅影响后续 TryReserve/Reservation 校验，
// 不回溯既有 committed/reserved。
func (p *Pool) SetMaxBytes(n int64) {
	p.mu.Lock()
	p.maxBytes = n
	p.mu.Unlock()
}

// UsageByBucket 从本池向下递归收集所有 Scope 的 committed 占用，key 为完整路径。
// 本池自身计入其路径 key（根池计入空路径 ""）。
func (p *Pool) UsageByBucket() map[string]int64 {
	m := make(map[string]int64)
	p.collect(m)
	return m
}

// newScope 创建路径为父路径拼接 path 的新子作用域，其底层账本挂到本池之下。
func (p *Pool) newScope(path string, maxBytes int64) *Scope {
	child := &Pool{
		maxBytes: maxBytes,
		parent:   p,
		path:     joinPath(p.path, path),
	}
	s := &Scope{pool: child}
	p.mu.Lock()
	p.children = append(p.children, s)
	p.mu.Unlock()
	return s
}

// Resolve 沿 children 段树按路径段逐级下探，返回**最深匹配**子作用域（http route 式）。
// 段序查找：每个匹配子层继续向下，直到某段无匹配子层为止；返回的节点即最长前缀命中点。
// 若路径上的层未装配（如 /a/b/c 仅装配到 /a/b），返回已命中的最深层（文件仍受该层上限
// 约束、committed 归集到该层）。对返回值调用 TryReserve 时，reserveUp 沿 parent 链逐层
// 各自独立检查 maxBytes——所有层级的配额约束自动完成，调用方无需理会层级。
// segs 为空时返回 p 自身对应的 Scope（供"桶内顶级文件"回落）。
func (p *Pool) Resolve(segs []string) *Scope {
	cur := p
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			break
		}
		cur = child.pool
	}
	return &Scope{pool: cur}
}

// ResolvePath 把路径按 "/" 拆段后调用 Resolve（便捷版）。
func (p *Pool) ResolvePath(path string) *Scope {
	return p.Resolve(strings.Split(path, "/"))
}

// EnsureScope 拆段逐级下探挂载：已存在的子层复用，不存在的现建（maxBytes 应用于最终层，
// 中间层 0 = 不限制）。返回最终段的 Scope。用于装配期把配置路径（如 user/videos/hd）
// 展开为多层嵌套子作用域。
func (p *Pool) EnsureScope(segs []string, maxBytes int64) *Scope {
	cur := p
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		child := cur.childWithSegment(seg)
		if child == nil {
			mb := int64(0)
			if i == len(segs)-1 {
				mb = maxBytes
			}
			child = cur.newScope(seg, mb)
		}
		cur = child.pool
	}
	return &Scope{pool: cur}
}

// childWithSegment 返回 path 末段恰为 seg 的直接子层 Scope（children 段序查找）。
// p 持有读锁时调用须注意：此处仅遍历快照读取，无并发下的结构性变更。
func (p *Pool) childWithSegment(seg string) *Scope {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, c := range p.children {
		if lastSegment(c.pool.path) == seg {
			return c
		}
	}
	return nil
}

// lastSegment 取路径最后一段（如 "/tenant/a/user/videos" → "videos"）。
func lastSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// collect 以快照方式递归收集各层 committed 到 m。
func (p *Pool) collect(m map[string]int64) {
	p.mu.RLock()
	committed, path, children := p.committed, p.path, p.children
	p.mu.RUnlock()
	m[path] += committed
	for _, c := range children {
		c.pool.collect(m)
	}
}

// reserveUp 沿父链从自身向上预留 n：每层在锁内检查上限并累加，父链失败则回滚自身累加。
func (p *Pool) reserveUp(n int64) error {
	p.mu.Lock()
	if p.maxBytes > 0 && p.committed+p.reserved+n > p.maxBytes {
		p.mu.Unlock()
		return ErrStorageFull
	}
	p.reserved += n
	p.mu.Unlock()
	if p.parent != nil {
		if err := p.parent.reserveUp(n); err != nil {
			p.mu.Lock()
			p.reserved -= n
			p.mu.Unlock()
			return err
		}
	}
	return nil
}

// commitUp 沿父链把预留 amount 对账为实际占用 actual（diff=actual−amount 可正可负）。
func (p *Pool) commitUp(amount, actual int64) {
	p.mu.Lock()
	p.reserved -= amount
	p.committed += actual
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.commitUp(amount, actual)
	}
}

// releaseUp 沿父链归还预留 n（放弃预留）。
func (p *Pool) releaseUp(n int64) {
	p.mu.Lock()
	p.reserved -= n
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.releaseUp(n)
	}
}

// releaseCommittedUp 沿父链释放已确认占用 n（文件删除按文件大小释放）。
func (p *Pool) releaseCommittedUp(n int64) {
	p.mu.Lock()
	p.committed -= n
	if p.committed < 0 {
		p.committed = 0
	}
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.releaseCommittedUp(n)
	}
}

// adjustUp 沿父链传播已确认占用增量 diff（可正可负，防下溢归零）。
func (p *Pool) adjustUp(diff int64) {
	p.mu.Lock()
	p.committed += diff
	if p.committed < 0 {
		p.committed = 0
	}
	p.mu.Unlock()
	if p.parent != nil {
		p.parent.adjustUp(diff)
	}
}

// available 计算可用额度：maxBytes<=0 返回 MaxInt64，否则 maxBytes−(committed+reserved)，负值归 0。
func (p *Pool) available() int64 {
	p.mu.RLock()
	maxBytes, used := p.maxBytes, p.committed+p.reserved
	p.mu.RUnlock()
	if maxBytes <= 0 {
		return math.MaxInt64
	}
	if used >= maxBytes {
		return 0
	}
	return maxBytes - used
}

// joinPath 拼接父子路径，处理空段与多余分隔符。
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	return strings.TrimRight(parent, "/") + "/" + strings.TrimLeft(child, "/")
}

// Scope 是配额子作用域（路径化叠加）。持有底层账本 *Pool，子池操作自动向父链聚合。
type Scope struct {
	pool *Pool
}

// Scope 返回该作用域下的子作用域（路径继续叠加；maxBytes 省略时 0 = 不限制）。
func (s *Scope) Scope(path string, maxBytes ...int64) *Scope {
	var mb int64
	if len(maxBytes) > 0 {
		mb = maxBytes[0]
	}
	return s.pool.newScope(path, mb)
}

// Resolve 沿本作用域 children 段树按路径段逐级下探（http route 式），返回最深匹配子作用域。
// 等价于 Pool.Resolve 在本层进行；segs 为空时返回自身。
func (s *Scope) Resolve(segs []string) *Scope {
	return s.pool.Resolve(segs)
}

// ResolvePath 便捷版：把路径按 "/" 拆段后 Resolve。
func (s *Scope) ResolvePath(path string) *Scope {
	return s.Resolve(strings.Split(path, "/"))
}

// EnsureScope 拆段逐级下探挂载到本作用域下（已存在复用，不存在现建），返回最终段 Scope。
func (s *Scope) EnsureScope(segs []string, maxBytes int64) *Scope {
	return s.pool.EnsureScope(segs, maxBytes)
}

// TryReserve 预留 estimate（仅增加 reserved，不落 committed）。失败返回 ErrStorageFull。
func (s *Scope) TryReserve(estimate int64) (*Reservation, error) {
	amt := nonNeg(estimate)
	if err := s.pool.reserveUp(amt); err != nil {
		return nil, err
	}
	return &Reservation{scope: s, amount: amt}, nil
}

// ReleaseUsage 释放已确认占用 n（文件删除时按文件大小释放）。
func (s *Scope) ReleaseUsage(n int64) {
	s.pool.releaseCommittedUp(nonNeg(n))
}

// Adjust 调整已确认占用（覆盖写同文件尺寸变化场景，diff 语义）。
// committed += (next − prev)，diff 可正可负，沿父链同步；不经过 reserved。
// 多文件桶下仅修正被覆盖文件的尺寸变化，不丢弃其它文件占用。
func (s *Scope) Adjust(prev, next int64) {
	s.pool.adjustUp(next - prev)
}

// Usage 返回已确认占用。
func (s *Scope) Usage() int64 { return s.pool.Usage() }

// Reserved 返回预留中。
func (s *Scope) Reserved() int64 { return s.pool.Reserved() }

// MaxBytes 返回上限（0 = 不限制）。
func (s *Scope) MaxBytes() int64 { return s.pool.maxBytes }

// Available 返回可用额度 = max − (committed+reserved)；max<=0 时返回 MaxInt64。
func (s *Scope) Available() int64 { return s.pool.available() }

// UsageByBucket 从该作用域向下按路径归集 committed 占用。
func (s *Scope) UsageByBucket() map[string]int64 {
	m := make(map[string]int64)
	s.pool.collect(m)
	return m
}

// Reservation 是预留句柄。Commit(actual) 按实际对账；Release() 放弃预留。
// 保证 Commit/Release 至多生效一次（重复调用忽略）。
type Reservation struct {
	scope  *Scope
	amount int64
	done   atomic.Bool
}

// Commit 按实际落地大小对账：预留 amount → 实际 actual（diff=actual−amount 同步父链）。
func (r *Reservation) Commit(actual int64) {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.scope.pool.commitUp(r.amount, nonNeg(actual))
}

// Release 放弃预留（归还 reserved）。
func (r *Reservation) Release() {
	if !r.done.CompareAndSwap(false, true) {
		return
	}
	r.scope.pool.releaseUp(r.amount)
}

// nonNeg 把负数归零（estimate/actual/释放量不允许为负）。
func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}
