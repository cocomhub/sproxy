// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// cold_test.go 锁定 M4「冷段**按需/随机**读取」：
//  ① 字节区间读（`OpenRange`）：只读所需区间，**不整文件加载**；
//  ② 条目流式 + **早停**：`limit` 满足即停，内存 O(一行)；
//  ③ 时间范围**整文件跳过**（`SkipByTime`）：`Since` 晚于段末条 ⇒ 不打开该文件；
//  ④ 枚举跳过归档半成品（`.tmp`），且带首/尾时间（有界探测）。

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeColdSegments 造两个「冷段」文件（手工写 JSON 行，模拟已归档的段）。
func writeColdSegments(t *testing.T) (dir string, tsList []time.Time) {
	t.Helper()
	dir = t.TempDir()
	base := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	write := func(name string, from int) {
		var b strings.Builder
		for i := range 3 {
			e := Entry{Scope: "system", Seq: uint64(from + i + 1), Kind: KindEvent,
				Action: "act", TS: base.Add(time.Duration(from+i) * time.Hour)}
			line, _ := json.Marshal(e)
			b.Write(line)
			b.WriteString("\n")
			tsList = append(tsList, e.TS)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("audit.log.2", 0) // 更旧（index 2）
	write("audit.log.1", 3) // 更新
	// 半成品（必须被枚举忽略）
	if err := os.WriteFile(filepath.Join(dir, "audit.log.9.tmp"), []byte("half"), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	return dir, tsList
}

// TestListArchive_ProbesTimesAndSkipsTmp 枚举冷段：带首/尾时间、跳过 .tmp、最旧优先。
func TestListArchive_ProbesTimesAndSkipsTmp(t *testing.T) {
	t.Parallel()
	dir, _ := writeColdSegments(t)
	segs, err := ListArchive(dir)
	if err != nil {
		t.Fatalf("ListArchive: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("应只枚举 2 个冷段（跳过 .tmp）, got %d", len(segs))
	}
	if segs[0].Index != 2 {
		t.Fatalf("应最旧优先（index 2 在前）, got %d", segs[0].Index)
	}
	for _, s := range segs {
		if s.First.IsZero() || s.Last.IsZero() || s.Last.Before(s.First) {
			t.Fatalf("首/尾时间探测异常: %+v", s)
		}
	}
}

// TestColdSegment_OpenRange 只读指定字节区间（随机读，不整文件加载）。
func TestColdSegment_OpenRange(t *testing.T) {
	t.Parallel()
	dir, _ := writeColdSegments(t)
	segs, err := ListArchive(dir)
	if err != nil || len(segs) == 0 {
		t.Fatalf("ListArchive: %v", err)
	}
	seg := segs[0]
	full, rerr := os.ReadFile(seg.Path)
	if rerr != nil {
		t.Fatalf("read full: %v", rerr)
	}
	// 取中间 40 字节，断言与整文件对应区间一致（证明是定点读）。
	const off, n = 20, 40
	rc, oerr := seg.OpenRange(off, n)
	if oerr != nil {
		t.Fatalf("OpenRange: %v", oerr)
	}
	defer rc.Close()
	part, _ := io.ReadAll(rc)
	if len(part) != n {
		t.Fatalf("区间长度=%d, want %d", len(part), n)
	}
	if string(part) != string(full[off:off+n]) {
		t.Fatalf("区间内容与整文件不一致")
	}
	// 越界区间自动截断（不 panic）。
	rc2, _ := seg.OpenRange(seg.Size-5, 100)
	defer rc2.Close()
	tail, _ := io.ReadAll(rc2)
	if len(tail) != 5 {
		t.Fatalf("越界应按剩余截断为 5, got %d", len(tail))
	}
}

// TestQueryColdStore_EarlyStopAndFilter 流式 + 过滤 + limit 早停。
func TestQueryColdStore_EarlyStopAndFilter(t *testing.T) {
	t.Parallel()
	dir, _ := writeColdSegments(t)
	store := LocalSegmentStore{Dir: dir}

	all, err := QueryColdStore(context.Background(), []SegmentStore{store}, EntryFilter{}, 0)
	if err != nil {
		t.Fatalf("QueryColdStore: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("应读到 2 段 × 3 条 = 6 条, got %d", len(all))
	}
	// 最旧优先：第一条应来自 audit.log.2（Seq 1）。
	if all[0].Seq != 1 {
		t.Fatalf("应按最旧段优先, first seq=%d", all[0].Seq)
	}
	// limit 早停
	two, err := QueryColdStore(context.Background(), []SegmentStore{store}, EntryFilter{}, 2)
	if err != nil || len(two) != 2 {
		t.Fatalf("limit=2 ⇒ 2 条, got %d err=%v", len(two), err)
	}
	// 过滤（Action 不匹配 ⇒ 0 条）
	none, _ := QueryColdStore(context.Background(), []SegmentStore{store}, EntryFilter{Action: "nope"}, 0)
	if len(none) != 0 {
		t.Fatalf("过滤应无命中, got %d", len(none))
	}
}

// TestSkipByTime_SkipsWholeSegment Since 晚于段末条 ⇒ 整文件跳过（不打开）。
func TestSkipByTime_SkipsWholeSegment(t *testing.T) {
	t.Parallel()
	dir, ts := writeColdSegments(t)
	segs, _ := ListArchive(dir)
	// 取全局最晚时间 + 1h ⇒ 所有段都应被判定为可跳过。
	var last time.Time
	for _, x := range ts {
		if x.After(last) {
			last = x
		}
	}
	f := EntryFilter{Since: last.Add(time.Hour)}
	skipped := 0
	for _, s := range segs {
		if s.SkipByTime(f) {
			skipped++
		}
	}
	if skipped != len(segs) {
		t.Fatalf("所有段都应可整文件跳过: skipped=%d total=%d", skipped, len(segs))
	}
	// 反向：无时间过滤 ⇒ 一律不跳过（保守读取）。
	for _, s := range segs {
		if s.SkipByTime(EntryFilter{}) {
			t.Fatal("无 Since 时不得跳过")
		}
	}
}
