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

// tailChunkBytes 是尾读的单次区间大小（冷段是追加写文件：最新条目在尾部）。
const tailChunkBytes = 256 << 10

// ReadTailCold 从**最新冷段**起、**从段尾向前**读，收集满足过滤的条目直到 limit，返回
// **按 TS 升序**（与 `QueryCold` 同序，便于与热段结果合并）。
//
// 为什么需要（评审 R13）：
//   - **正确性**：`QueryCold` 逐段**从头**读且「收满即停」⇒ 最新段条目数超过预算时，返回的是
//     该段**最旧**的 N 条（最新条目被预算截掉）⇒ newest-first + limit 的结果集错误；
//   - **有界性**：尾读只读最新段尾部若干块，不扫历史段。
//
// 实现要点：按段内最后一条时间**倒序**处理；每段只取「还缺多少条」（`need`）为止，
// 因此 `limit` 小 ⇒ 只读一块；过滤不匹配时才继续向前扩展（最坏与旧实现同级）。
func ReadTailCold(ctx context.Context, fs syncpkg.FS, roots []string, f EntryFilter, limit int) ([]Entry, error) {
	if fs == nil {
		return nil, fmt.Errorf("audit: ReadTailCold 需要 pkg/sync.FS")
	}
	if limit <= 0 {
		return nil, nil
	}
	segs, err := newestFirstColdSegments(ctx, fs, roots)
	if err != nil {
		return nil, err
	}
	var (
		got      []Entry // 保持「新 → 旧」
		firstErr error
	)
	for _, seg := range segs {
		if len(got) >= limit {
			break
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if coldSegmentBeforeSince(seg, f) {
			continue // 整段早于 Since 过滤 ⇒ 跳过（无需 IO）
		}
		batch, berr := tailEntries(ctx, fs, seg, f, limit-len(got)) // 升序，最多 need 条
		if berr != nil && firstErr == nil {
			// R14 NEW-4：读失败不得静默截断（部分结果仍返回，错误上抛给调用方告警）。
			firstErr = berr
		}
		slices.Reverse(batch) // → 降序
		got = append(got, batch...)
	}
	if len(got) > limit {
		got = got[:limit]
	}
	slices.Reverse(got) // 降序 → 全局升序
	return got, firstErr
}

// newestFirstColdSegments 汇总所有 root 的冷段，按段内最后一条时间**倒序**（无额外 IO：
// ListCold 已探测 First/Last）。
func newestFirstColdSegments(ctx context.Context, fs syncpkg.FS, roots []string) ([]ColdSegment, error) {
	var segs []ColdSegment
	for _, root := range roots {
		part, err := ListCold(ctx, fs, root)
		if err != nil {
			return nil, err
		}
		segs = append(segs, part...)
	}
	sort.SliceStable(segs, func(i, k int) bool {
		if !segs[i].Last.Equal(segs[k].Last) {
			return segs[i].Last.After(segs[k].Last)
		}
		return segs[i].Path > segs[k].Path
	})
	return segs, nil
}

// coldSegmentBeforeSince 判定整段都早于 `Since` 过滤（可跳过 IO）。
func coldSegmentBeforeSince(seg ColdSegment, f EntryFilter) bool {
	return seg.Size > 0 && !seg.Last.IsZero() && !f.Since.IsZero() && !seg.Last.After(f.Since)
}

// tailEntries 从段尾向前按块读取，返回该段内**满足过滤、最多 need 条、升序**的条目。
// 读够 need 条即停（不会为了一段把整段读完）。
//
// **跨块行**（评审 R14 C11）：块边界可能落在某一行的中间。若只丢「块首残行」（旧实现），
// 该行的两半会分属两个块 —— 新块里作为**首元素**被丢、旧块里作为**末元素**缺换行解码失败
// ⇒ **整条消失**（每跨一个边界丢 1 条）。这里用 `carry` 把较新块的首残行（该行的**尾段**）
// 传给下一（更旧）块：其**末行**正是同一行的首段 ⇒ 拼接后解码。
func tailEntries(ctx context.Context, fs syncpkg.FS, seg ColdSegment, f EntryFilter, need int) ([]Entry, error) {
	if seg.Size <= 0 || need <= 0 {
		return nil, nil
	}
	rr := syncpkg.AssertRangeReader(fs)
	var (
		got   []Entry // 新 → 旧
		carry []byte  // 较新块的首残行（跨块行的尾段）
	)
	end := seg.Size
	for end > 0 && len(got) < need {
		off := max(end-tailChunkBytes, 0)
		chunk, cerr := readColdChunk(ctx, fs, rr, seg, off, end)
		if cerr != nil {
			// R14 NEW-4：读失败必须上抛（此前 break 后返回部分结果且无错误 ⇒ 静默截断）。
			return finishTailEntries(got, need), cerr
		}
		batch, nextCarry := chunkEntriesDesc(chunk, off > 0, carry, f, need-len(got))
		got = append(got, batch...)
		carry = nextCarry
		if off == 0 {
			break
		}
		end = off
	}
	return finishTailEntries(got, need), nil
}

// finishTailEntries 截断到 need 并反转为升序（新→旧 ⇒ 升序）。
func finishTailEntries(got []Entry, need int) []Entry {
	if len(got) > need {
		got = got[:need]
	}
	slices.Reverse(got)
	return got
}

// chunkEntriesDesc 把一段区间字节解码为**降序**（新→旧）条目，最多 need 条；返回本块首残行
// （`partialFirst=true` 时）供更旧的块拼接。
//
// partialFirst=true 表示区间起点落在行中 ⇒ 本块首元素是该行的**尾段**（不可单独解码），
// 作为 carry 交给下一块；若上一块留下了 carry，则本块**末元素**是同一行的**首段**，拼接后解码。
func chunkEntriesDesc(chunk []byte, partialFirst bool, carry []byte, f EntryFilter, need int) ([]Entry, []byte) {
	lines := strings.Split(string(chunk), "\n")
	var nextCarry []byte
	if partialFirst {
		nextCarry = []byte(lines[0])
		lines = lines[1:]
	}
	out := make([]Entry, 0, min(len(lines), max(need, 0)))
	// 跨块行：本块末元素 + 上一块的 carry（末元素不带换行 ⇒ 拼上 carry 与换行后完整）。
	if len(carry) > 0 && len(lines) > 0 && lines[len(lines)-1] != "" && len(out) < need {
		joined := lines[len(lines)-1] + string(carry)
		if e, ok := decodeSegmentLine([]byte(joined + "\n")); ok && f.Match(e) {
			out = append(out, e)
		}
		lines = lines[:len(lines)-1]
	}
	for i := len(lines) - 1; i >= 0 && len(out) < need; i-- {
		if lines[i] == "" {
			continue
		}
		if e, ok := decodeSegmentLine([]byte(lines[i] + "\n")); ok && f.Match(e) {
			out = append(out, e)
		}
	}
	return out, nextCarry
}

// readColdChunk 读冷段 [off, end) 区间；无区间能力时整段读后切分（能力探测回落，不静默返回空）。
func readColdChunk(ctx context.Context, fs syncpkg.FS, rr syncpkg.RangeReader, seg ColdSegment, off, end int64) ([]byte, error) {
	if rr != nil {
		b, _, err := syncpkg.RangeReadAll(rr, ctx, seg.Path, off, end-off)
		if err != nil {
			return nil, fmt.Errorf("audit: 冷段区间读 %s [%d,%d) 失败: %w", seg.Path, off, end, err)
		}
		return b, nil
	}
	rc, oerr := fs.OpenRead(ctx, seg.Path)
	if oerr != nil {
		return nil, fmt.Errorf("audit: 冷段打开 %s 失败: %w", seg.Path, oerr)
	}
	b, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return nil, fmt.Errorf("audit: 冷段读取 %s 失败: %w", seg.Path, rerr)
	}
	if int64(len(b)) < end {
		return nil, fmt.Errorf("audit: 冷段 %s 短读（size=%d < 请求末尾 %d）", seg.Path, len(b), end)
	}
	return b[off:end], nil
}

// PurgeCold 删除**最后一条时间早于 cutoff** 的冷段，返回删除数量。
//
// 为什么需要（评审 R13 P2）：审计 TTL（`retention.audit_ttl`）此前只重写**在线段**；
// 一旦开启归档（`archive_keep=0` 时封段随即搬走），热段里根本没有超龄数据 ⇒ TTL 实际不生效，
// 超期证据永久留在冷存储（磁盘只增不减 + 保留期不成立）。
//
// 保守语义：**时间未知**（首/尾探测失败）的段**绝不删除**（宁多留不误删）；
// 单个删除失败记录首个错误并继续（其余照删）。
func PurgeCold(ctx context.Context, fs syncpkg.FS, root string, cutoff time.Time) (int, error) {
	segs, err := ListCold(ctx, fs, root)
	if err != nil {
		return 0, err
	}
	var (
		n     int
		first error
	)
	for _, seg := range segs {
		if cerr := ctx.Err(); cerr != nil {
			return n, cerr
		}
		if seg.Last.IsZero() || !seg.Last.Before(cutoff) {
			continue
		}
		if derr := fs.Delete(ctx, seg.Path); derr != nil {
			if first == nil {
				first = fmt.Errorf("audit: 删除过期冷段 %s 失败: %w", seg.Path, derr)
			}
			continue
		}
		n++
	}
	return n, first
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
