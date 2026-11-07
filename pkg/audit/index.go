// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"sync"
)

// index.go 是**内存活跃索引**（设计 §4.2/§5.4.1）：加速读取（Web UI / 快速回看），
// 有界（容量满则环形覆盖最旧），并支持按谓词逐出（按龄清理）。
//
// 与 Journal 的关系：Journal 是权威持久化；Index 是**热副本**。
// 逐出策略（设计 E4）：优先逐出「已持久化」条目 —— 由 Service 负责判定（见 service.go），
// 本文件只提供结构与查询语义。

// Index 是内存活跃索引。
type Index interface {
	// Add 追加条目（容量满时环形覆盖最旧）。
	Add(e Entry)
	// Query 按过滤条件返回（**最新在前**：与 /api/audit 倒序契约一致）。
	Query(f EntryFilter) []Entry
	// Len 返回当前条数。
	Len() int
	// ApproxBytes 返回近似占用字节（有界保留判据）。
	ApproxBytes() int64
	// Evict 逐出最旧的 n 条（返回被逐出条目）。
	Evict(n int) []Entry
	// Prune 按谓词逐出（按龄清理；返回逐出条数）。
	Prune(keep func(Entry) bool) int
}

// NullIndex 是 Index 的 null-object（禁用剖面）。
type NullIndex struct{}

func (NullIndex) Add(Entry)                  {}
func (NullIndex) Query(EntryFilter) []Entry  { return nil }
func (NullIndex) Len() int                   { return 0 }
func (NullIndex) ApproxBytes() int64         { return 0 }
func (NullIndex) Evict(int) []Entry          { return nil }
func (NullIndex) Prune(func(Entry) bool) int { return 0 }

// RingIndex 是有界环形索引（容量满则覆盖最旧）。
type RingIndex struct {
	mu       sync.RWMutex
	buf      []Entry
	head     int // 下一个写入位置
	size     int
	capacity int
	bytes    int64 // 近似占用
}

// NewRingIndex 创建容量为 capacity 的环形索引（capacity<=0 ⇒ 空实现语义，Add 静默丢弃）。
func NewRingIndex(capacity int) *RingIndex {
	if capacity < 0 {
		capacity = 0
	}
	return &RingIndex{buf: make([]Entry, capacity), capacity: capacity}
}

// Capacity 返回容量（0 = 禁用）。
func (r *RingIndex) Capacity() int { return r.capacity }

// Add 追加条目（容量满时覆盖最旧）。
func (r *RingIndex) Add(e Entry) {
	if r == nil || r.capacity == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == r.capacity {
		// 覆盖最旧：先扣减其占用。
		r.bytes -= approxEntryBytes(r.buf[r.head])
	} else {
		r.size++
	}
	r.buf[r.head] = e
	r.bytes += approxEntryBytes(e)
	r.head = (r.head + 1) % r.capacity
	if r.bytes < 0 {
		r.bytes = 0
	}
}

// Query 返回命中的条目（**最新在前**），limit<=0 时返回全部命中。
func (r *RingIndex) Query(f EntryFilter) []Entry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.capacity == 0 || r.size == 0 {
		return nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = r.size
	}
	out := make([]Entry, 0, min(limit, r.size))
	// 倒序遍历：最新元素下标是 (head-1+capacity)%capacity。
	for i := 0; i < r.size && len(out) < limit; i++ {
		idx := (r.head - 1 - i + r.capacity*2) % r.capacity
		if f.Match(r.buf[idx]) {
			out = append(out, r.buf[idx])
		}
	}
	return out
}

// Len 返回当前条数。
func (r *RingIndex) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// ApproxBytes 返回近似占用。
func (r *RingIndex) ApproxBytes() int64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bytes
}

// Evict 逐出最旧的 n 条（返回被逐出条目，最旧在前）。
func (r *RingIndex) Evict(n int) []Entry {
	if r == nil || n <= 0 {
		return nil
	}
	// ⚠️ `size` 必须在锁内读：Evict/Prune/Add 都会在锁内写它，锁外读 = data race
	// （CI -race 实测：`Evict` 的 guard 与另一 goroutine 的 `r.size -= n` 并发 ✗）。
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return nil
	}
	if n > r.size {
		n = r.size
	}
	// 最旧元素 = (head - size + capacity) % capacity。
	oldest := (r.head - r.size + r.capacity*2) % r.capacity
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		idx := (oldest + i) % r.capacity
		out = append(out, r.buf[idx])
		r.bytes -= approxEntryBytes(r.buf[idx])
		var zero Entry
		r.buf[idx] = zero
	}
	r.size -= n
	if r.bytes < 0 {
		r.bytes = 0
	}
	return out
}

// Prune 按谓词逐出（keep=false 的条目被移除；返回逐出条数）。
// 语义：保持剩余条目的相对顺序（重建内部环）。
func (r *RingIndex) Prune(keep func(Entry) bool) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size == 0 {
		return 0
	}
	oldest := (r.head - r.size + r.capacity*2) % r.capacity
	kept := make([]Entry, 0, r.size)
	var dropped int
	var bytes int64
	for i := 0; i < r.size; i++ {
		e := r.buf[(oldest+i)%r.capacity]
		if keep(e) {
			kept = append(kept, e)
			bytes += approxEntryBytes(e)
		} else {
			dropped++
		}
	}
	for i := range r.buf {
		var zero Entry
		r.buf[i] = zero
	}
	copy(r.buf, kept)
	r.size = len(kept)
	r.head = r.size % r.capacity
	r.bytes = bytes
	return dropped
}

// approxEntryBytes 估算条目占用（用于内存上限判据；常量系数足够作为上界判据）。
func approxEntryBytes(e Entry) int64 {
	const overhead = 160
	n := int64(overhead)
	n += int64(len(e.Scope) + len(e.Action) + len(e.Actor) + len(e.Owner) + len(e.Mesh) +
		len(e.ObjType) + len(e.Object) + len(e.Detail) + len(e.Volume) + len(e.RequestID) +
		len(e.SourceIP) + len(e.Step) + len(e.Err) + len(e.Node))
	for k, v := range e.Meta {
		n += int64(len(k))
		if s, ok := v.(string); ok {
			n += int64(len(s))
		} else {
			n += 16
		}
	}
	return n
}
