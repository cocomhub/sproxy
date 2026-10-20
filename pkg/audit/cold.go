// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// cold.go 是 **冷段（已归档段）读取**（M4：跨热/冷段 Query 的冷侧）。
//
// 两条用户裁定（2026-10-20）决定了这里的形状：
//  1. **复用既有能力**：归档与「云端下载 → 转存」同类 ⇒ 冷段存放/读取都走 **`pkg/sync.FS`**
//     （本地目录 / 外部卷 / 加密卷 / 远端同一套语义），**不自造 store/sink 接口** ✗；
//  2. **按需/随机读取**：必须能只读所需片段 —— 用 FS 的可选能力 **`RangeReader`**（`OpenRangeRead`）
//     做**字节区间读**；条目级读取用**流式 + 早停**（内存 O(一行)，与段大小无关 ✓）。
//
// 冷段与热段**同格式**（都是 Journal 段文件）⇒ 解码复用 `decodeSegmentLine`（单一实现 ✓）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ColdSegment 描述一个**已归档**的段文件（含时间范围，用于整文件级跳过）。
//
// First/Last 由 `ListCold` **有界**探测；零值表示探测失败/空段（调用方应保守读取，不跳过）。
type ColdSegment struct {
	Path  string    // FS 内相对路径
	Index int       // 归档序号（`audit.log.3` ⇒ 3；越大越旧）
	Size  int64     // 字节数
	First time.Time // 首条条目时间
	Last  time.Time // 末条条目时间
}

// tailProbeBytes 是探测末条时间的尾部窗口（足以覆盖一条普通条目）。
const tailProbeBytes = 64 << 10

// segmentNameMatches 报告文件名是否属于审计段（任一布局）。
//
// 三种命名：`audit.log`（全局热段）/ `audit.log.N`（全局封段）/ `<scope>.audit.log[.N]`（每作用域）。
func segmentNameMatches(name string) bool {
	if strings.HasSuffix(name, ".tmp") {
		return false // 归档半成品（tmp + rename）不得被读
	}
	return strings.Contains(name, "audit.log")
}

// ListCold **递归**枚举 `fs` 上 `root` 下的冷段，最旧优先（目录 asc = 日期序，段序号 desc）。
//
// 只做 Stat + 有界首/尾探测 ⇒ 每文件少量小读，不把内容读进内存 ✓。
func ListCold(ctx context.Context, fs syncpkg.FS, root string) ([]ColdSegment, error) {
	if fs == nil {
		return nil, fmt.Errorf("audit: ListCold 需要 pkg/sync.FS")
	}
	ents, err := syncpkg.WalkEntries(ctx, fs, root, true, false, nil)
	if err != nil {
		if syncpkg.IsNotExist(err) {
			return nil, nil // 尚无冷段：不是错误
		}
		return nil, fmt.Errorf("audit: 枚举冷段失败: %w", err)
	}
	out := make([]ColdSegment, 0, len(ents))
	for _, e := range ents {
		if e.IsDir {
			continue
		}
		name := path.Base(e.Path)
		if !segmentNameMatches(name) {
			continue
		}
		seg := ColdSegment{Path: e.Path, Size: e.Size, Index: archiveIndex(name)}
		seg.First, seg.Last = probeSegmentTimes(ctx, fs, seg.Path, seg.Size)
		out = append(out, seg)
	}
	sort.SliceStable(out, func(i, k int) bool {
		di, dk := path.Dir(out[i].Path), path.Dir(out[k].Path)
		if di != dk {
			return di < dk
		}
		return out[i].Index > out[k].Index
	})
	return out, nil
}

// probeSegmentTimes 有界探测段的首/末条目时间（失败 ⇒ 零值，调用方保守读取）。
func probeSegmentTimes(ctx context.Context, fs syncpkg.FS, p string, size int64) (first, last time.Time) {
	first = probeFirstTS(ctx, fs, p)
	if size <= 0 {
		return first, first
	}
	last = probeLastTS(ctx, fs, p, size)
	if last.IsZero() {
		last = first
	}
	return first, last
}

// probeFirstTS 区间读前 64KiB 取首条时间（FS 无区间能力时退化为流式读首行）。
func probeFirstTS(ctx context.Context, fs syncpkg.FS, p string) time.Time {
	if rr := syncpkg.AssertRangeReader(fs); rr != nil {
		if b, _, err := syncpkg.RangeReadAll(rr, ctx, p, 0, 64<<10); err == nil {
			if e, ok := firstEntry(b); ok {
				return e.TS
			}
			return time.Time{}
		}
	}
	rc, err := fs.OpenRead(ctx, p)
	if err != nil {
		return time.Time{}
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	if sc.Scan() {
		if e, ok := decodeScannedLine(sc); ok {
			return e.TS
		}
	}
	return time.Time{}
}

// probeLastTS 区间读尾部窗口取末条时间（只取小段，远端也只传输该窗口 ✓）。
func probeLastTS(ctx context.Context, fs syncpkg.FS, p string, size int64) time.Time {
	rr := syncpkg.AssertRangeReader(fs)
	if rr == nil {
		return time.Time{}
	}
	off := max(size-tailProbeBytes, 0)
	b, _, err := syncpkg.RangeReadAll(rr, ctx, p, off, size-off)
	if err != nil {
		return time.Time{}
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if line == "" {
			continue
		}
		if e, ok := decodeSegmentLine([]byte(line + "\n")); ok {
			return e.TS
		}
	}
	return time.Time{}
}

// firstEntry 从字节窗口解析第一条完整行。
func firstEntry(b []byte) (Entry, bool) {
	idx := strings.IndexByte(string(b), '\n')
	if idx < 0 {
		return Entry{}, false
	}
	return decodeSegmentLine(b[:idx+1])
}

// SkipByTime 报告该冷段是否可以**整文件跳过**（时间未知 ⇒ 不跳过）。
func (c ColdSegment) SkipByTime(f EntryFilter) bool {
	if f.Since.IsZero() || c.Last.IsZero() {
		return false
	}
	return !c.Last.After(f.Since)
}

// OpenRange 用 FS 的 **`RangeReader`** 能力读字节区间（远端卷也只传输所需区间 ✓）。
//
// FS 未实现该能力 ⇒ 返回 `syncpkg.ErrUnsupported`（调用方应显式处置，**不得**静默整文件下载 ✗）。
func OpenRange(ctx context.Context, fs syncpkg.FS, seg ColdSegment, off, n int64) (io.ReadCloser, error) {
	if fs == nil {
		return nil, fmt.Errorf("audit: 需要 pkg/sync.FS")
	}
	rr := syncpkg.AssertRangeReader(fs)
	if rr == nil {
		return nil, fmt.Errorf("audit: 该 FS 不支持区间读（%w）", syncpkg.ErrUnsupported)
	}
	if n <= 0 || (seg.Size > 0 && off+n > seg.Size) {
		n = seg.Size - off
	}
	if n < 0 {
		n = 0
	}
	return rr.OpenRangeRead(ctx, seg.Path, off, n)
}

// StreamColdSegment **流式**读取一个冷段：逐行解码 → 过滤 → fn；fn 返回 false 即早停。
//
// 用 `OpenRead` 全程流式（内存 O(一行)）——需要「只取某段字节」时用 `OpenRange` ✓。
func StreamColdSegment(ctx context.Context, fs syncpkg.FS, seg ColdSegment, f EntryFilter, fn func(Entry) bool) error {
	if fn == nil || fs == nil {
		return nil
	}
	rc, err := fs.OpenRead(ctx, seg.Path)
	if err != nil {
		return fmt.Errorf("audit: 打开冷段失败: %w", err)
	}
	defer rc.Close()
	return scanEntries(ctx, rc, f, fn)
}

// scanEntries 是冷段流式读取的**唯一实现**。
func scanEntries(ctx context.Context, r io.Reader, f EntryFilter, fn func(Entry) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, ok := decodeScannedLine(sc)
		if !ok || !f.Match(e) {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	if serr := sc.Err(); serr != nil {
		return fmt.Errorf("audit: 读冷段失败: %w", serr)
	}
	return nil
}

// decodeScannedLine 解码 bufio.Scanner 取出的行。
//
// `decodeSegmentLine` 是 Journal 的**共享**解码入口，要求行尾换行（其自身调用方传入整行含换行）；
// Scanner 会剥掉换行 ⇒ 这里补回后复用同一实现（**解码格式只有一份** ✓）。
func decodeScannedLine(sc *bufio.Scanner) (Entry, bool) {
	b := sc.Bytes()
	if len(b) == 0 {
		return Entry{}, false
	}
	buf := make([]byte, len(b)+1)
	copy(buf, b)
	buf[len(b)] = '\n'
	return decodeSegmentLine(buf)
}

// QueryCold 在 `fs` 的 `roots` 上查询冷段（最旧优先、流式、早停、按时间整文件跳过）。
//
// **收敛点**：热段走 `FileJournal.Read`，冷段走本函数；两者共用 `EntryFilter.Match` +
// `decodeSegmentLine` ⇒ 过滤/解码只有一份实现 ✓。
func QueryCold(ctx context.Context, fs syncpkg.FS, roots []string, f EntryFilter, limit int) ([]Entry, error) {
	var out []Entry
	for _, root := range roots {
		segs, err := ListCold(ctx, fs, root)
		if err != nil {
			return nil, err
		}
		for _, seg := range segs {
			if seg.SkipByTime(f) {
				continue
			}
			limitHit := false
			sErr := StreamColdSegment(ctx, fs, seg, f, func(e Entry) bool {
				out = append(out, e)
				limitHit = limit > 0 && len(out) >= limit
				return !limitHit
			})
			if sErr != nil {
				return nil, sErr
			}
			if limitHit {
				return out, nil
			}
		}
	}
	return out, nil
}
