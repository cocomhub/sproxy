// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// prod_hardening_test.go 钉住 2026-10-21 对抗审查的两条 P1：
//
//  1. `FileJournal.ReadTail` 段序写反 ⇒ 热目录仍有封段时 `/api/audit?limit=N` 返回**最旧**的 N 条；
//  2. 冷段「末条时间探测失败」被回退成首条时间 ⇒ 保留期内的证据被整段删除。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestFileJournal_ReadTail_ReturnsNewestAmongSealedSegments 多封段共存时，ReadTail 必须返回最新条目。
func TestFileJournal_ReadTail_ReturnsNewestAmongSealedSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileJournal(FileJournalConfig{
		Dir:      dir,
		Layout:   LayoutGlobal,
		SyncMode: SyncNone,
		// 极小段上限 ⇒ 每条都触发轮转，热目录里同时留多个封段（`.1` 最新）。
		MaxSize:     1,
		MaxArchives: 64,
		Clock:       systemClock{},
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	const total = 8
	for i := range total {
		if aerr := j.Append(Entry{
			Node: "n1", Seq: uint64(i + 1), Scope: "system", Kind: KindEvent,
			Action: fmt.Sprintf("act-%02d", i), Type: "sensitive", Result: "success",
			TS: time.Unix(1_700_000_000+int64(i), 0).UTC(),
		}); aerr != nil {
			t.Fatalf("Append %d: %v", i, aerr)
		}
	}
	if err := j.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, rerr := j.ReadTail(context.Background(), EntryFilter{}, 3)
	if rerr != nil {
		t.Fatalf("ReadTail: %v", rerr)
	}
	if len(got) != 3 {
		t.Fatalf("ReadTail 条数 = %d, want 3", len(got))
	}
	want := []string{"act-05", "act-06", "act-07"} // 升序返回的最新 3 条
	for i, w := range want {
		if got[i].Action != w {
			t.Fatalf("第 %d 条 = %q, want %q（全量=%v；段序写反会返回最旧条目）",
				i, got[i].Action, w, actionsOf(got))
		}
	}
}

func actionsOf(es []Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Action)
	}
	return out
}

// failTailRangeFS 让「非零起点」的区间读失败（模拟远端/无 Range 能力时末条探测失败），
// 而首条探测（offset=0）仍然可用。
type failTailRangeFS struct {
	syncpkg.FS
}

func (f *failTailRangeFS) OpenRangeRead(ctx context.Context, p string, off, length int64) (io.ReadCloser, error) {
	if off > 0 {
		return nil, fmt.Errorf("failTailRangeFS: 模拟末条探测失败（off=%d）", off)
	}
	return syncpkg.AssertRangeReader(f.FS).OpenRangeRead(ctx, p, off, length)
}

// TestProbeSegmentTimes_LastUnknownStaysUnknown 末条探测失败时 Last 必须为零值（不得伪造成首条时间）。
func TestProbeSegmentTimes_LastUnknownStaysUnknown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := time.Unix(1_700_000_000, 0).UTC() // 8 天前
	const segName = "audit.log.1.aaaaaaaaaaaaaaaa"
	segPath := writeTwoTimeSegment(t, dir, segName, old, old.Add(7*24*time.Hour))
	st, serr := os.Stat(segPath)
	if serr != nil {
		t.Fatalf("stat: %v", serr)
	}

	fsys := &failTailRangeFS{FS: syncpkg.NewLocalFS(dir, nil)}
	first, last := probeSegmentTimes(context.Background(), fsys, segName, st.Size())
	if first.IsZero() {
		t.Fatal("首条探测应成功（前提）")
	}
	if !last.IsZero() {
		t.Fatalf("末条探测失败时 Last 必须保持零值（got %v；旧实现回退成首条时间 ⇒ 保留期内证据被删）", last)
	}

	// 端到端语义：PurgeCold 的保留期判据必须**保守保留**未知末条时间的段。
	cutoff := old.Add(24 * time.Hour) // 段内仍有新于 cutoff 的条目 ⇒ 不得删
	n, perr := PurgeCold(context.Background(), fsys, "", cutoff)
	if perr != nil {
		t.Fatalf("PurgeCold: %v", perr)
	}
	if n != 0 {
		t.Fatalf("末条时间未知的段不得删除, got %d", n)
	}
	if _, serr := os.Stat(segPath); serr != nil {
		t.Fatalf("保留期内的冷段必须保留: %v", serr)
	}
}

// writeTwoTimeSegment 写一个段：首条 ts1、大量填充分隔后末条 ts2（段体积 > tailProbeBytes）。
func writeTwoTimeSegment(t *testing.T, dir, name string, ts1, ts2 time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	writeLine := func(ts time.Time, action string) {
		line, err := json.Marshal(Entry{
			Scope: "system", Kind: KindEvent, Action: action,
			Type: "sensitive", Result: "success", TS: ts,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	writeLine(ts1, "oldest")
	pad := strings.Repeat("p", 800)
	for i := range 100 { // ≈80KiB > tailProbeBytes(64KiB)
		writeLine(ts1, fmt.Sprintf("%s-%03d", pad, i))
	}
	writeLine(ts2, "newest")

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestMaxSeqInSegmentFile_CrossChunk 反向扫描必须正确处理**跨块行**（> chunk 的大段），
// 且只返回该作用域的最大 Seq（seqFloor 的有界实现，避免全量解析 1GiB 段导致 OOM）。
func TestMaxSeqInSegmentFile_CrossChunk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")

	var b strings.Builder
	appendEntry := func(scope ScopeID, seq uint64, pad int) {
		e := Entry{
			Scope: scope, Kind: KindEvent, Action: "act", Type: "sensitive",
			Result: "success", Seq: seq, TS: time.Unix(1_700_000_000, 0).UTC(),
			Meta: map[string]any{"pad": strings.Repeat("x", pad)},
		}
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	// ≈700KiB（跨 256KiB 块多次），目标作用域的最大 Seq 落在靠近尾部但非末条。
	for i := range 700 {
		appendEntry("other", uint64(i+1), 900)
	}
	appendEntry("target", 41, 900)
	appendEntry("target", 77, 900) // 最大
	appendEntry("other", 9999, 900)
	if err := os.WriteFile(p, []byte(b.String()), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, ok := maxSeqInSegmentFile(p, "target")
	if !ok || got != 77 {
		t.Fatalf("maxSeqInSegmentFile = (%d, %v), want (77, true)", got, ok)
	}
	if _, ok := maxSeqInSegmentFile(p, "absent"); ok {
		t.Fatal("不存在的作用域不应命中")
	}
}
