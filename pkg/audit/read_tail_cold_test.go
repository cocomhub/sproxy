// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// read_tail_cold_test.go 锁定评审 R13 的两件事：
//  1. **正确性**：newest-first + limit 时，冷段必须返回**最新**条目（旧实现 `QueryCold`
//     是「最旧优先 + 收满即停」⇒ 返回最旧 N 条，最新条目被预算截掉）；
//  2. **有界性**：尾读只读段尾区间（用计数的 RangeReader 断言读取字节数 < 段大小）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeColdSeg 写一个含 count 条连续时间戳条目的冷段（升序追加）。
func writeColdSeg(t *testing.T, dir, name string, start time.Time, count int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b bytes.Buffer
	for i := 0; i < count; i++ {
		line, err := json.Marshal(Entry{
			Scope: "system", Kind: KindEvent, Action: "tail_probe",
			Type: "sensitive", Result: "success", Object: fmt.Sprintf("%s-%d", name, i),
			TS: start.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b.Bytes(), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestReadTailCold_ReturnsNewest 跨多段时返回**最新**的 limit 条（升序）。
func TestReadTailCold_ReturnsNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)

	base := time.Unix(1_800_000_000, 0).UTC()
	writeColdSeg(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", base, 5)                             // 最旧
	writeColdSeg(t, dir, "audit.log.2.bbbbbbbbbbbbbbbb", base.Add(time.Hour), 5)              // 较新
	lastSeg := writeColdSeg(t, dir, "audit.log.3.cccccccccccccccc", base.Add(2*time.Hour), 5) // 最新

	got, err := ReadTailCold(ctx, fsys, []string{""}, EntryFilter{}, 3)
	if err != nil {
		t.Fatalf("ReadTailCold: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应返回 3 条, got %d", len(got))
	}
	// 升序 + 最后一条必须等于最新段的最后一条（对象名可判定）。
	for i := 1; i < len(got); i++ {
		if got[i].TS.Before(got[i-1].TS) {
			t.Fatalf("结果必须按 TS 升序, 第 %d 条乱序", i)
		}
	}
	want := filepath.Base(lastSeg) + "-4"
	if got[2].Object != want {
		t.Fatalf("最新条应为 %q（尾读方向错）, got %q", want, got[2].Object)
	}
}

// TestReadTailCold_MatchesQueryCold 大 limit 时与「最旧优先扫描」结果集一致（等价性）。
func TestReadTailCold_MatchesQueryCold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	base := time.Unix(1_800_000_000, 0).UTC()
	writeColdSeg(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", base, 4)
	writeColdSeg(t, dir, "audit.log.2.bbbbbbbbbbbbbbbb", base.Add(time.Hour), 4)

	want, err := QueryCold(ctx, fsys, []string{""}, EntryFilter{}, 0)
	if err != nil {
		t.Fatalf("QueryCold: %v", err)
	}
	got, err := ReadTailCold(ctx, fsys, []string{""}, EntryFilter{}, len(want)+10)
	if err != nil {
		t.Fatalf("ReadTailCold: %v", err)
	}
	// `QueryCold` 的**段序**来自 ListCold（段索引倒序），不保证全局时间序 ⇒ 比较集合（按 TS 排序后）。
	if len(got) != len(want) {
		t.Fatalf("结果条数不一致: tail=%d query=%d", len(got), len(want))
	}
	byTS := func(es []Entry) []Entry {
		out := append([]Entry(nil), es...)
		sort.Slice(out, func(i, k int) bool { return out[i].TS.Before(out[k].TS) })
		return out
	}
	gq, wq := byTS(got), byTS(want)
	for i := range wq {
		if !gq[i].TS.Equal(wq[i].TS) || gq[i].Object != wq[i].Object {
			t.Fatalf("第 %d 条不一致: tail=%v/%q query=%v/%q", i, gq[i].TS, gq[i].Object, wq[i].TS, wq[i].Object)
		}
	}
}

// countingRangeFS 包装 FS，统计 OpenRangeRead 读取的字节数（验证尾读有界）。
type countingRangeFS struct {
	syncpkg.FS
	read int64
}

func (c *countingRangeFS) OpenRangeRead(ctx context.Context, path string, off, size int64) (io.ReadCloser, error) {
	rr := syncpkg.AssertRangeReader(c.FS)
	if rr == nil {
		return nil, syncpkg.ErrUnsupported
	}
	c.read += size
	return rr.OpenRangeRead(ctx, path, off, size)
}

// TestReadTailCold_ReadsBounded 单段远大于 limit 时，尾读只读尾部若干块（远小于段大小）。
func TestReadTailCold_ReadsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	fsys := &countingRangeFS{FS: inner}

	base := time.Unix(1_800_000_000, 0).UTC()
	p := writeColdSeg(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", base, 8000) // 约 1MB+
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	got, err := ReadTailCold(ctx, fsys, []string{""}, EntryFilter{}, 2)
	if err != nil {
		t.Fatalf("ReadTailCold: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条, got %d", len(got))
	}
	if fsys.read >= st.Size() {
		t.Fatalf("尾读必须小于整段: read=%d size=%d", fsys.read, st.Size())
	}
	if !strings.HasSuffix(got[1].Object, "-7999") {
		t.Fatalf("最后一条应是段尾最后一条, got %q", got[1].Object)
	}
}
