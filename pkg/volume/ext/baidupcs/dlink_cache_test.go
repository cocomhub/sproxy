// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"testing"
	"time"
)

// TestDlinkLRU_BasicGetPutDelete：基本 get/put/delete。
func TestDlinkLRU_BasicGetPutDelete(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k1", "u1", time.Hour)
	if u, ok := c.get("k1"); !ok || u != "u1" {
		t.Fatalf("get 应命中 u1，got %q ok=%v", u, ok)
	}
	c.delete("k1")
	if _, ok := c.get("k1"); ok {
		t.Fatal("delete 后不应命中")
	}
}

// TestDlinkLRU_EvictsOldest：超容量淘汰最久未用。
func TestDlinkLRU_EvictsOldest(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(3)
	c.put("a", "ua", time.Hour)
	c.put("b", "ub", time.Hour)
	c.put("c", "uc", time.Hour)
	c.get("b") // 命中 b ⇒ b 变最新
	c.put("d", "ud", time.Hour)
	if _, ok := c.get("a"); ok {
		t.Fatal("a 应被 LRU 淘汰")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.get(k); !ok {
			t.Fatalf("%s 不应被淘汰", k)
		}
	}
}

// TestDlinkLRU_ExpiredRemoved：过期条目按 TTL 失效并从索引移除。
// 直接把条目 exp 改到过去（不 Sleep——R14 睡眠棘轮约束，且消除真实时钟 flake）。
func TestDlinkLRU_ExpiredRemoved(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k", "u", time.Hour)
	c.mu.Lock()
	c.idx["k"].Value.(*dlinkEntry).exp = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, ok := c.get("k"); ok {
		t.Fatal("过期条目不应命中")
	}
	if c.len() != 0 {
		t.Fatalf("过期条目应从索引移除，len=%d", c.len())
	}
}

// TestDlinkLRU_UpdateRefreshes：同 key 更新刷新过期时间与位次。
func TestDlinkLRU_UpdateRefreshes(t *testing.T) {
	t.Parallel()
	c := newDlinkLRU(4)
	c.put("k", "u1", time.Hour)
	c.mu.Lock()
	c.idx["k"].Value.(*dlinkEntry).exp = time.Now().Add(-time.Second) // 人为置过期
	c.mu.Unlock()
	c.put("k", "u2", time.Hour) // 同 key 更新应刷新 exp
	if u, ok := c.get("k"); !ok || u != "u2" {
		t.Fatalf("刷新后应命中 u2，got %q ok=%v", u, ok)
	}
}
