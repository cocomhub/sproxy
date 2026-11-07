// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// audit_max_total_test.go 锁定 M1-8（设计 docs/designs/2026-10-19-audit-service.md §5.4 / T8 / E15）：
// `max_total_bytes` 是**审计全部段（热 + 归档）的总字节硬上限**（用户要求 ⑩：审计落盘纳入配额，
// 防磁盘打满）。语义：
//  ① 超上限 ⇒ 先删**最旧归档**（有界保留；计入 IncrEvict —— 受控淘汰，不是丢失）；
//  ② 归档删尽仍放不下（如热段本身已超上限）⇒ **有界丢弃**新条目（计入 IncrDrop；
//     Append 返回 nil —— 审计是 best-effort，绝不因满盘阻塞业务）；
//  ③ 不配置（0）⇒ 不限制（零回归）。

import (
	"testing"
	"time"
)

// maxTotalMetrics 只记录 drop/evict（**嵌入 NullMetrics** ⇒ 其余方法零值，
// 不必逐一实现 Metrics 接口；与 core_test.go 的 countingMetrics 区分命名）。
type maxTotalMetrics struct {
	NullMetrics
	evict int
	drop  int
}

func (m *maxTotalMetrics) IncrDrop(n int)  { m.drop += n }
func (m *maxTotalMetrics) IncrEvict(n int) { m.evict += n }

// TestFileJournal_MaxTotalBytes_PrunesOldestArchive 超上限 ⇒ 删最旧归档（有界保留）。
func TestFileJournal_MaxTotalBytes_PrunesOldestArchive(t *testing.T) {
	t.Parallel()
	m := &maxTotalMetrics{}
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), MaxSize: 240, MaxArchives: 8, MaxTotalBytes: 1200,
		SyncMode: SyncNone, Metrics: m, Clock: systemClock{},
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()

	for i := range 40 {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "test", TS: time.Now(), Detail: "0123456789"}); aerr != nil {
			t.Fatalf("Append #%d: %v", i, aerr)
		}
	}
	if m.evict == 0 {
		t.Fatal("超上限应淘汰最旧归档（IncrEvict 应 > 0）")
	}
	segs, serr := j.Segments()
	if serr != nil {
		t.Fatalf("Segments: %v", serr)
	}
	var total int64
	for _, s := range segs {
		total += s.Size
	}
	// 允许超出「一个段」（写入前治理：现有段不超上限，新条目总会有落点）。
	if total > 1200+2*240 {
		t.Fatalf("总字节应收敛到上限附近, got %d（max_total=1200）", total)
	}
}

// TestFileJournal_MaxTotalBytes_DropsWhenCannotFit 删尽归档仍放不下 ⇒ 有界丢弃（不报错、不长盘）。
func TestFileJournal_MaxTotalBytes_DropsWhenCannotFit(t *testing.T) {
	t.Parallel()
	m := &maxTotalMetrics{}
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), MaxTotalBytes: 80, SyncMode: SyncNone, Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()

	if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "first", TS: time.Now(), Detail: "0123456789"}); aerr != nil {
		t.Fatalf("首条应写入（此时总量为 0）: %v", aerr)
	}
	segs, _ := j.Segments()
	var before int64
	for _, s := range segs {
		before += s.Size
	}
	// 后续条目：总量（> 80）已超上限且无归档可删 ⇒ 丢弃（不报错、文件不增长）。
	for range 5 {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "later", TS: time.Now(), Detail: "0123456789"}); aerr != nil {
			t.Fatalf("有界丢弃不得返回错误（best-effort）: %v", aerr)
		}
	}
	if m.drop == 0 {
		t.Fatal("放不下时应计 IncrDrop（有界丢弃）")
	}
	segs, _ = j.Segments()
	var after int64
	for _, s := range segs {
		after += s.Size
	}
	if after != before {
		t.Fatalf("被丢弃的条目不得落盘（文件不应增长）: before=%d after=%d", before, after)
	}
}

// TestFileJournal_MaxTotalBytes_ZeroMeansUnlimited 0 ⇒ 不限制（零回归）。
func TestFileJournal_MaxTotalBytes_ZeroMeansUnlimited(t *testing.T) {
	t.Parallel()
	m := &maxTotalMetrics{}
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), MaxSize: 200, MaxArchives: 2, SyncMode: SyncNone, Metrics: m,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()
	for i := range 30 {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "test", TS: time.Now(), Detail: "0123456789"}); aerr != nil {
			t.Fatalf("Append #%d: %v", i, aerr)
		}
	}
	if m.drop != 0 || m.evict != 0 {
		t.Fatalf("未配置上限时不得丢弃/淘汰: drop=%d evict=%d", m.drop, m.evict)
	}
}
