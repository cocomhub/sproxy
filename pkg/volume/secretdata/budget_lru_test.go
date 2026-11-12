// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"reflect"
	"sort"
	"testing"
)

// 用固定 10 字节/条 的计量，便于用整数心智校验淘汰边界。
func sizeOf10(b []byte) int64 { return int64(len(b)) }

// TestBudgetLRU_EvictsOldestOnCap 锁定「超预算从 LRU 尾端淘汰」这一核心语义
// （blockletCache 与 metaEntryCache 共用 ⇒ 改坏会同时影响两者）。
func TestBudgetLRU_EvictsOldestOnCap(t *testing.T) {
	t.Parallel()
	c := newBudgetLRU[[]byte](30, 0, sizeOf10) // 预算 30 字节 = 3 条
	c.put("a", make([]byte, 10))
	c.put("b", make([]byte, 10))
	c.put("c", make([]byte, 10))
	// 触达 a ⇒ 变成最近使用；再插入 d ⇒ 应淘汰最久未用的 b。
	if _, ok := c.get("a"); !ok {
		t.Fatal("a 应命中")
	}
	evicted := c.put("d", make([]byte, 10))
	if len(evicted) != 1 || evicted[0] != "b" {
		t.Fatalf("应淘汰最久未用的 b，得到 %v", evicted)
	}
	if _, ok := c.get("b"); ok {
		t.Fatal("b 应已被淘汰")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s 应仍在缓存", k)
		}
	}
}

// TestBudgetLRU_RejectsOversizeSingleItem 锁定「单条超预算直接拒收」：
// 否则会把预算内其它条目全部挤掉而自身仍超限。
func TestBudgetLRU_RejectsOversizeSingleItem(t *testing.T) {
	t.Parallel()
	c := newBudgetLRU[[]byte](20, 0, sizeOf10)
	c.put("keep", make([]byte, 10))
	if ev := c.put("huge", make([]byte, 100)); ev != nil {
		t.Fatalf("单条超预算不应淘汰其它条目，却淘汰了 %v", ev)
	}
	if _, ok := c.get("huge"); ok {
		t.Fatal("超预算条目不应入缓存")
	}
	if _, ok := c.get("keep"); !ok {
		t.Fatal("原条目应保持不动")
	}
}

// TestBudgetLRU_ReputUpdatesAccounting 锁定「重复 put 同一 key」的计量与队列正确性
// （先扣旧字节并移出队列，再按新值计入）。
func TestBudgetLRU_ReputUpdatesAccounting(t *testing.T) {
	t.Parallel()
	c := newBudgetLRU[[]byte](50, 0, sizeOf10)
	c.put("a", make([]byte, 10))
	c.put("a", make([]byte, 30))
	if len(c.order) != 1 {
		t.Fatalf("重复 put 不应产生重复队列项，order=%v", c.order)
	}
	if c.bytes != 30 {
		t.Fatalf("计量应更新为 30，得到 %d", c.bytes)
	}
}

// TestBudgetLRU_InvalidatePrefixAndClear 锁定清理语义（前缀失效 / 全清）。
func TestBudgetLRU_InvalidatePrefixAndClear(t *testing.T) {
	t.Parallel()
	c := newBudgetLRU[[]byte](1000, 0, sizeOf10)
	c.put("d1/f1", make([]byte, 10))
	c.put("d1/f2", make([]byte, 10))
	c.put("d2/f3", make([]byte, 10))
	c.invalidatePrefix("d1/")
	keys := append([]string(nil), c.order...)
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"d2/f3"}) {
		t.Fatalf("d1/ 前缀应全部失效，剩余 %v", keys)
	}
	if c.bytes != 10 {
		t.Fatalf("计量应扣减为 10，得到 %d", c.bytes)
	}
	c.clear()
	if c.bytes != 0 || len(c.items) != 0 || len(c.order) != 0 {
		t.Fatal("clear 后应完全归零")
	}
}

// TestBudgetLRU_NilReceiverIsSafe 锁定「缓存关闭（nil）」时全部方法安全（恒 miss / 无操作）。
func TestBudgetLRU_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var c *budgetLRU[[]byte]
	if _, ok := c.get("x"); ok {
		t.Fatal("nil 缓存应恒 miss")
	}
	if ev := c.put("x", make([]byte, 10)); ev != nil {
		t.Fatal("nil 缓存 put 应无操作")
	}
	c.invalidatePrefix("x")
	c.clear()
	if got := newBudgetLRU[[]byte](-1, 64, sizeOf10); got != nil {
		t.Fatal("budget<0 应返回 nil（关闭缓存）")
	}
}
