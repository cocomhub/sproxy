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
	for i := range count {
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

// TestReadTailCold_CrossChunkLine R14 C11：**跨 256KiB 块的条目不得丢失**。
//
// 构造是**可判定的**：先算出「段尾 256KiB」边界，并让某条大对象条目的行恰好跨过它
// （把尾部填充量裁到 tailChunkBytes - len(big) + len(big)/2）。旧实现把该行的两半分别丢掉 ⇒
// 整条消失。
func TestReadTailCold_CrossChunkLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	base := time.Unix(1_800_000_000, 0).UTC()

	mk := func(object string, ts time.Time) []byte {
		line, err := json.Marshal(Entry{
			Scope: "system", Kind: KindEvent, Action: "cross_probe",
			Type: "sensitive", Result: "success", Object: object, TS: ts,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return append(line, '\n')
	}

	head := bytes.Buffer{}
	for i := range 5 {
		head.Write(mk(fmt.Sprintf("head-%d", i), base.Add(time.Duration(i)*time.Second)))
	}
	bigObject := "big:" + strings.Repeat("x", 4<<10)
	bigLine := mk(bigObject, base.Add(100*time.Second))

	// 尾部填充量：使 boundary = S - tailChunkBytes 落在 bigLine 内。
	tailNeed := tailChunkBytes - len(bigLine) + len(bigLine)/2
	tail := bytes.Buffer{}
	for i := 0; tail.Len() < tailNeed; i++ {
		tail.Write(mk(fmt.Sprintf("tail-%d:%s", i, strings.Repeat("y", 512)), base.Add(time.Duration(200+i)*time.Second)))
	}
	// 裁掉超出部分（按整行裁不了就保留——只要 boundary 仍落在 bigLine 内即可，下面用断言校验）。
	content := append(append(head.Bytes(), bigLine...), tail.Bytes()...)
	if err := os.WriteFile(filepath.Join(dir, "audit.log.1.aaaaaaaaaaaaaaaa"), content, 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 前提断言：边界确实落在 bigLine 内部（否则用例无效）。
	boundary := int64(len(content)) - tailChunkBytes
	bigStart := int64(head.Len())
	if boundary <= bigStart || boundary >= bigStart+int64(len(bigLine)) {
		t.Fatalf("前置失败：边界 %d 未落在 big 条目 [%d,%d) 内（用例无效）",
			boundary, bigStart, bigStart+int64(len(bigLine)))
	}

	got, err := ReadTailCold(ctx, fsys, []string{""}, EntryFilter{}, 1000)
	if err != nil {
		t.Fatalf("ReadTailCold: %v", err)
	}
	found := false
	for _, e := range got {
		if strings.HasPrefix(e.Object, "big:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("跨块条目丢失（C11 未修）：返回 %d 条但缺少 big 条目", len(got))
	}
}
