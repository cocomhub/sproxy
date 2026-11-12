// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// mkMeta 造一个含 n 个 blocklet 的 meta（用于计量与淘汰断言）。
func mkMeta(n int) *shardseal.Meta {
	return &shardseal.Meta{
		Version: 1,
		Chunks:  []shardseal.ChunkInfo{{Blocklets: make([]shardseal.BlockletInfo, n)}},
	}
}

// TestMetaSizeBytes_ScalesWithBlocklets 锁定计量口径：按 blocklet/window 条目数 ×191B
// （191 是 2026-11-12 真实 meta 实测系数：430,652 blocklets ⇒ 78 MB 常驻）。
func TestMetaSizeBytes_ScalesWithBlocklets(t *testing.T) {
	t.Parallel()
	small := metaSizeBytes(mkMeta(10))
	big := metaSizeBytes(mkMeta(1000))
	if small >= big {
		t.Fatalf("计量应随 blocklet 数增长：small=%d big=%d", small, big)
	}
	if got := metaSizeBytes(mkMeta(1000)); got < 1000*191 {
		t.Fatalf("1000 blocklets 至少应计 %d 字节，得到 %d", 1000*191, got)
	}
	if metaSizeBytes(nil) != 0 {
		t.Fatal("nil meta 应计 0")
	}
}

// TestMetaEntryCache_EvictsOnBudget 锁定 L1b 的淘汰：预算只够一条时，第二条挤掉第一条。
func TestMetaEntryCache_EvictsOnBudget(t *testing.T) {
	t.Parallel()
	// 预算刚好容纳 1000 个 blocklet 的 meta（1000*191+4096 = 195096）。
	c := newMetaEntryCache(195096 + 10)
	m1, m2 := mkMeta(1000), mkMeta(1000)
	c.put("a", m1)
	evicted := c.put("b", m2)
	if len(evicted) != 1 || evicted[0] != "a" {
		t.Fatalf("应淘汰 a，得到 %v", evicted)
	}
	if _, ok := c.get("b"); !ok {
		t.Fatal("b 应在缓存内")
	}
	if _, ok := c.get("a"); ok {
		t.Fatal("a 应已被淘汰")
	}
}

// TestMetaEntryCache_SingleOversizeRejected 单条超预算应拒收（否则会把预算内其它条目全挤掉）。
func TestMetaEntryCache_SingleOversizeRejected(t *testing.T) {
	t.Parallel()
	c := newMetaEntryCache(195096) // 只够一条
	c.put("keep", mkMeta(1000))
	if ev := c.put("huge", mkMeta(100000)); ev != nil {
		t.Fatalf("超预算条目不应挤掉其它条目，却淘汰了 %v", ev)
	}
	if _, ok := c.get("keep"); !ok {
		t.Fatal("原条目应保持不动")
	}
}

// TestClearEvictedMetaLocked 锁定 P2 最关键的不变量：
// **被 L1b 淘汰的条目必须清空 `meta`**，否则强引用使其脱离预算约束 ⇒ 常驻量无限增长（回到 OOM）。
//
// 变异：删掉 `old.meta = nil` ⇒ 本用例红。
func TestClearEvictedMetaLocked(t *testing.T) {
	t.Parallel()
	fs := &SecretdataFS{index: map[string]*metaEntry{
		"a": {meta: mkMeta(10)},
		"b": {meta: mkMeta(10)},
		"c": {meta: mkMeta(10)},
	}}
	fs.mu.Lock()
	fs.clearEvictedMetaLocked([]string{"a", "b", "nonexistent"}, fs.index["c"])
	fs.mu.Unlock()

	if fs.index["a"].meta != nil || fs.index["b"].meta != nil {
		t.Fatal("被淘汰条目的 meta 必须置空（否则脱离 L1b 上限约束）")
	}
	if fs.index["c"].meta == nil {
		t.Fatal("未在淘汰列表中的条目不应被动")
	}
	if _, ok := fs.index["a"]; !ok {
		t.Fatal("条目本身应保留在索引（只是回到未加载态）")
	}
}

// TestClearEvictedMetaLocked_SkipsSelf 锁定 self 排除：同 rel 重新 put 时不该清掉自己刚挂上的实例。
func TestClearEvictedMetaLocked_SkipsSelf(t *testing.T) {
	t.Parallel()
	self := &metaEntry{meta: mkMeta(10)}
	fs := &SecretdataFS{index: map[string]*metaEntry{"a": self}}
	fs.mu.Lock()
	fs.clearEvictedMetaLocked([]string{"a"}, self)
	fs.mu.Unlock()
	if self.meta == nil {
		t.Fatal("self 不应被自己这一次 put 的淘汰清掉")
	}
}
