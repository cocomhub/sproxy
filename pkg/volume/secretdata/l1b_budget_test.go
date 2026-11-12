// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// l1b_budget_test.go 量化「L1b 预算 vs 批量操作命中率」—— 这是预算取值与
// 设计文档 §4.3「同类批量操作第二次全命中」是否成立的**实测依据**。
//
// 背景（实测）：单个大文件 meta 常驻 78 MB（430,652 blocklets）；「全量扫描/批量读 meta」
// 是最大的批量场景。若热集总量 > 预算，则第二轮遍历**不可能全命中** —— 本用例把这个
// 边界钉死，避免文档里的乐观假设误导后续优化。

import (
	"fmt"
	"testing"
)

// TestL1B_BudgetDeterminesBatchHitRate 锁定：**第二轮命中数 = floor(预算 / 单条大小)**，
// 与「这一批是否超过预算」直接相关。
//
// 决定性判据：命中数必须**恰好等于**容量（不多不少），从而证明淘汰是按字节预算精确发生的；
// 只断"有命中"或"命中率>0"都会被「缓存几乎失效」的实现骗过。
func TestL1B_BudgetDeterminesBatchHitRate(t *testing.T) {
	// sproxy:serial: 构造较大的 meta 夹具（每条 ~19 MB），避免并行放大内存
	const perMetaBlocklets = 100000 // 100000 × 191 B ≈ 19.1 MB
	one := metaSizeBytes(mkMeta(perMetaBlocklets))
	budget := int64(defaultMetaCacheBytes) // 256 MiB（默认值）
	capacity := budget / one               // 预算恰好能容纳的条数
	if capacity < 2 {
		t.Fatalf("夹具不合适：单条 %.1f MB，容量仅 %d", float64(one)/(1<<20), capacity)
	}
	n := int(capacity) + 6 // 故意超出预算

	c := newMetaEntryCache(budget)
	// 第一轮：模拟「批量读 meta」（全部加载 ⇒ 超出预算的部分会被淘汰）。
	for i := range n {
		c.put(fmt.Sprintf("d/f%d.mp4", i), mkMeta(perMetaBlocklets))
	}
	// 第二轮：统计命中（不 put ⇒ 不再产生淘汰）。
	hit := 0
	for i := range n {
		if _, ok := c.get(fmt.Sprintf("d/f%d.mp4", i)); ok {
			hit++
		}
	}
	t.Logf("单条≈%.1f MB · 预算=%d MiB · 容量=%d · 批量=%d ⇒ 第二轮命中=%d (%.0f%%)",
		float64(one)/(1<<20), budget>>20, capacity, n, hit, float64(hit)*100/float64(n))
	if hit != int(capacity) {
		t.Fatalf("命中数应恰等于容量 %d（按字节预算精确淘汰），实际 %d", capacity, hit)
	}
}

// TestL1B_SingleOversizeEvictsOthers 锁定**危险边界**：单条超过预算时，
// `budgetLRU` 拒收（不挤掉其它条目）⇒ 该文件每轮都要重新加载（命中率 0）。
// 这正是「把预算调到小于最大单文件」时的真实后果，必须先知道。
func TestL1B_SingleOversizeEvictsOthers(t *testing.T) {
	t.Parallel()
	small := metaSizeBytes(mkMeta(1000))
	c := newMetaEntryCache(small*2 + 1) // 预算 = 2 条小 meta
	c.put("small", mkMeta(1000))
	// 塞一个远超预算的大 meta（模拟真实 78 MB 单文件 vs 小预算）
	c.put("huge", mkMeta(100000))
	if _, ok := c.get("huge"); ok {
		t.Fatal("单条超预算必须被拒收（否则会把预算内其它条目全挤掉）")
	}
	if _, ok := c.get("small"); !ok {
		t.Fatal("拒收超预算条目时不应影响既有条目")
	}
}
