// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import "sync"

// budgetLRU 是**按字节预算淘汰的 LRU**（key 为字符串，值为任意类型）。
//
// 为什么抽出来（2026-11-12，P2）：本包原先只有块级明文缓存（`blockletCache`）需要
// 「字节预算 + LRU 淘汰」，现在元数据缓存（`metaEntryCache`，L1b）需要**完全相同的淘汰语义**。
// 按「同一语义只留一份实现」的要求，这里做成泛型实现，两者共用；`blockletCache` 的
// **对外 API 与行为保持不变**（薄包装），因此既有调用点零改动。
//
// 淘汰语义（与改造前的 `blockletCache` 逐条一致）：
//   - 超出预算时从 **LRU 尾端**（最久未用）淘汰；
//   - **单条超预算直接拒收**（否则会把预算内其它条目全部挤掉而自身仍超限）；
//   - 重复 put 同一 key ⇒ 先扣旧字节并移出队列，再按新值计入。
//
// `put` **返回被淘汰的 key 列表**（而非用回调）：回调会在持本结构锁时执行，调用方若
// 需要回到自己的索引上做清理，就会形成「本锁 → 外部锁」的反向顺序 ⇒ 有死锁风险。
// 返回列表让**调用方在它自己已持有的锁下**处理，锁顺序始终单向。
type budgetLRU[V any] struct {
	mu     sync.Mutex
	items  map[string]V
	order  []string // 前端最近使用
	bytes  int64    // 当前占用字节（由 sizeOf 计量）
	budget int64    // 字节预算
	sizeOf func(V) int64
}

// newBudgetLRU 创建预算 LRU；budget<=0 时用 def（def<=0 ⇒ 返回 nil，表示缓存关闭）。
func newBudgetLRU[V any](budget, def int64, sizeOf func(V) int64) *budgetLRU[V] {
	if budget < 0 {
		return nil
	}
	if budget == 0 {
		budget = def
	}
	if budget <= 0 {
		return nil
	}
	return &budgetLRU[V]{items: map[string]V{}, budget: budget, sizeOf: sizeOf}
}

// get 取缓存并触达 LRU 前端。nil 接收者（缓存关闭）恒 miss。
func (c *budgetLRU[V]) get(key string) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.touchLocked(key)
	return v, true
}

// put 写缓存，返回**被淘汰的 key**（调用方负责清理自己那边的关联状态）。
func (c *budgetLRU[V]) put(key string, v V) []string {
	if c == nil {
		return nil
	}
	sz := c.sizeOf(v)
	if sz > c.budget { // 单条超预算 ⇒ 拒收（避免把预算内其它条目全挤掉）
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.bytes -= c.sizeOf(old)
		delete(c.items, key)
		c.removeOrderLocked(key)
	}
	var evicted []string
	for c.bytes+sz > c.budget && len(c.order) > 0 {
		victim := c.order[len(c.order)-1]
		c.order = c.order[:len(c.order)-1]
		if old, ok := c.items[victim]; ok {
			c.bytes -= c.sizeOf(old)
			delete(c.items, victim)
			evicted = append(evicted, victim)
		}
	}
	c.items[key] = v
	c.bytes += sz
	c.order = append([]string{key}, c.order...)
	return evicted
}

// invalidatePrefix 使某前缀下的条目全部失效（如某目录被整体删除/改名）。
func (c *budgetLRU[V]) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.items {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			c.bytes -= c.sizeOf(c.items[k])
			delete(c.items, k)
			c.removeOrderLocked(k)
		}
	}
}

// clear 清空缓存并归零计量。
func (c *budgetLRU[V]) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]V{}
	c.order = nil
	c.bytes = 0
}

// touchLocked 把 key 提到 LRU 前端（调用方持锁）。
func (c *budgetLRU[V]) touchLocked(key string) {
	for i := range c.order {
		if c.order[i] == key {
			copy(c.order[1:i+1], c.order[:i])
			c.order[0] = key
			return
		}
	}
}

// removeOrderLocked 从 LRU 序列中移除 key（调用方持锁）。
func (c *budgetLRU[V]) removeOrderLocked(key string) {
	for i := range c.order {
		if c.order[i] == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}
