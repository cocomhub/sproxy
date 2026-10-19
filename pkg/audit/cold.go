// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// cold.go 是 **冷段（已归档段）读取**（M4：跨热/冷段 Query 的冷侧）。
//
// 设计目标（用户 2026-10-20 明示）：**归档后的内容必须支持按需/随机读取**，不得为了看一段就
// 必须整文件下载。三层能力（使用方按场景选择，互不强制）：
//
//  1. `ListArchive`：枚举冷段 + **有界**探测首/尾行时间（首行 + 末尾窗口）⇒ 可按时间范围
//     **跳过整个文件**（不必打开全部冷段）；
//  2. `ColdSegment.OpenRange`：**字节级随机读**（`io.SectionReader`）⇒ O(1) seek + 只读所需区间；
//  3. `StreamColdSegment` / `StreamFrom`：**条目流式**读取（逐行解码 + 谓词早停，内存 O(一行)）。
//
// 冷段与热段**同格式**（都是 Journal 段文件）⇒ 解码复用 `decodeSegmentLine`（单一实现 ✓）。
// 远端/加密卷的冷段由装配层用 `pkg/sync` 的 `FS`（`ListDir`/`Stat`/**`RangeReader`**）实现
// 同一套 `SegmentStore` 接口 ✓ —— 审计包不依赖任何存储实现 ✓。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// ColdSegment 描述一个**已归档**的段文件（含时间范围，用于整文件级跳过）。
//
// First/Last 由 `ListArchive` **有界**探测；零值表示探测失败/空段（调用方应保守读取，不跳过）。
type ColdSegment struct {
	Path  string    // 路径
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

// ListArchive 枚举冷段目录下的段文件，按**最旧优先**排序（序号大者更旧）。
//
// 只做 Stat + 首/尾行探测（有界读）⇒ 即使成千上万冷段也是「每文件少量小读」，不把内容读进内存。
func ListArchive(dir string) ([]ColdSegment, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("audit: ListArchive 需要目录")
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 尚无冷段：不是错误（归档关闭/尚未轮转）
		}
		return nil, fmt.Errorf("audit: 枚举冷段失败: %w", err)
	}
	var out []ColdSegment
	// **递归**：归档按 `<prefix>/<UTC 日期>/<段名>` 落盘（见 SinkArchiver）⇒ 必须下探日期子目录。
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个坏项不阻断枚举（冷段目录可能被外部改动）
		}
		if d.IsDir() || !segmentNameMatches(d.Name()) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		seg := ColdSegment{Path: path, Size: info.Size(), Index: archiveIndex(d.Name())}
		seg.First, seg.Last = probeSegmentTimes(path, seg.Size)
		out = append(out, seg)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("audit: 枚举冷段失败: %w", walkErr)
	}
	// **最旧优先**：日期目录（YYYYMMDD）字典序即时间序 ⇒ 先按路径、再按段序号降序。
	sort.SliceStable(out, func(i, k int) bool {
		di, dk := filepath.Dir(out[i].Path), filepath.Dir(out[k].Path)
		if di != dk {
			return di < dk // 日期目录（YYYYMMDD）字典序 = 时间序（旧在前）
		}
		return out[i].Index > out[k].Index // 同目录内序号大者更旧
	})
	return out, nil
}

// probeSegmentTimes 有界探测段的首/末条目时间（失败 ⇒ 零值，调用方保守读取）。
func probeSegmentTimes(path string, size int64) (first, last time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// 首行
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	if sc.Scan() {
		if e, ok := decodeScannedLine(sc); ok {
			first = e.TS
		}
	}
	if size <= 0 {
		return first, first
	}

	// 末行：读尾部窗口，取最后一个完整行
	off := max(size-tailProbeBytes, 0)
	buf := make([]byte, size-off)
	if _, rerr := f.ReadAt(buf, off); rerr != nil && rerr != io.EOF {
		return first, first
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if line == "" {
			continue
		}
		if e, ok := decodeSegmentLine([]byte(line + "\n")); ok {
			last = e.TS
			break
		}
	}
	if last.IsZero() {
		last = first
	}
	return first, last
}

// sectionReadCloser 把 `io.SectionReader` 与底层文件配对，Close 时**同时**关闭文件。
//
// 为什么不用 `io.NopCloser`：它吞掉 Close ⇒ 文件句柄泄漏 ✗（实测会让临时目录清理失败，
// 且长跑进程会耗尽 fd）。
type sectionReadCloser struct {
	r io.Reader
	c io.Closer
}

func (s sectionReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s sectionReadCloser) Close() error { return s.c.Close() }

// OpenRange 返回该冷段的**字节区间**读取器（O(1) seek，不整文件加载 ✓）。
//
// n <= 0 表示读到文件末尾；越界由 `io.SectionReader` 自动截断。
func (c ColdSegment) OpenRange(off, n int64) (io.ReadCloser, error) {
	if off < 0 {
		return nil, fmt.Errorf("audit: 冷段区间偏移不能为负: %d", off)
	}
	f, err := os.Open(c.Path)
	if err != nil {
		return nil, fmt.Errorf("audit: 打开冷段失败: %w", err)
	}
	remain := max(c.Size-off, 0)
	if n <= 0 || n > remain {
		n = remain
	}
	return sectionReadCloser{r: io.NewSectionReader(f, off, n), c: f}, nil
}

// SkipByTime 报告该冷段是否可以**整文件跳过**（利用 First/Last；时间未知 ⇒ 不跳过）。
func (c ColdSegment) SkipByTime(f EntryFilter) bool {
	if f.Since.IsZero() || c.Last.IsZero() {
		return false
	}
	return !c.Last.After(f.Since)
}

// StreamColdSegment **流式**读取一个冷段：逐行解码 → 过滤 → fn；fn 返回 false 即早停。
func StreamColdSegment(ctx context.Context, c ColdSegment, f EntryFilter, fn func(Entry) bool) error {
	if fn == nil {
		return nil
	}
	fh, err := os.Open(c.Path)
	if err != nil {
		return fmt.Errorf("audit: 打开冷段失败: %w", err)
	}
	defer fh.Close()
	return scanEntries(ctx, fh, f, fn)
}

// scanEntries 是冷段流式读取的**唯一实现**（本地文件与 SegmentStore 共用）。
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

// SegmentStore 是冷段的**读取能力**（本地目录 / 外部卷 / 加密卷由装配层适配）。
//
// 契约：`OpenRange` 必须支持**任意字节区间**（远端卷用 `sync.RangeReader` 实现）⇒ 调用方只读
// 所需区间，绝不整文件下载 ✓。
type SegmentStore interface {
	// List 枚举冷段（实现应带 First/Last 以便按时间跳过整文件；未知可留零值 ⇒ 保守读取）。
	List(ctx context.Context) ([]ColdSegment, error)
	// OpenRange 打开 [off, off+n) 区间（n<=0 ⇒ 到末尾）。
	OpenRange(ctx context.Context, seg ColdSegment, off, n int64) (io.ReadCloser, error)
}

// LocalSegmentStore 是本地目录适配器（远端/加密卷由装配层用 `sync.FS` + `RangeReader` 实现同一接口）。
type LocalSegmentStore struct{ Dir string }

// List 实现 SegmentStore（本地枚举 + 有界首/尾探测）。
func (l LocalSegmentStore) List(context.Context) ([]ColdSegment, error) {
	return ListArchive(l.Dir)
}

// OpenRange 实现 SegmentStore（本地 SectionReader：O(1) seek ✓）。
func (l LocalSegmentStore) OpenRange(_ context.Context, seg ColdSegment, off, n int64) (io.ReadCloser, error) {
	return seg.OpenRange(off, n)
}

// StreamFrom 流式读取该段（逐行 + 谓词早停；内存 O(一行)）。
func StreamFrom(ctx context.Context, store SegmentStore, seg ColdSegment, f EntryFilter, fn func(Entry) bool) error {
	if store == nil || fn == nil {
		return nil
	}
	rc, err := store.OpenRange(ctx, seg, 0, 0)
	if err != nil {
		return err
	}
	defer rc.Close()
	return scanEntries(ctx, rc, f, fn)
}

// QueryColdStore 在若干 `SegmentStore` 上查询（最旧优先、流式、早停、按时间跳过整文件）。
//
// **收敛点**：热段走 `FileJournal.Read`，冷段走本函数；两者共用 `EntryFilter.Match` +
// `decodeSegmentLine` ⇒ 过滤/解码只有一份实现 ✓。
func QueryColdStore(ctx context.Context, stores []SegmentStore, f EntryFilter, limit int) ([]Entry, error) {
	var out []Entry
	for _, st := range stores {
		hit, err := queryOneStore(ctx, st, f, limit, &out)
		if err != nil {
			return nil, err
		}
		if hit {
			return out, nil
		}
	}
	return out, nil
}

// queryOneStore 查一个 SegmentStore（按段最旧优先 + 流式早停），把结果追加到 out。
// 返回 hit=true 表示已达 limit（调用方应立即返回）。拆出只为把 QueryColdStore 的复杂度留在阈值内。
func queryOneStore(ctx context.Context, st SegmentStore, f EntryFilter, limit int, out *[]Entry) (hit bool, err error) {
	if st == nil {
		return false, nil
	}
	segs, err := st.List(ctx)
	if err != nil {
		return false, err
	}
	for _, seg := range segs {
		if seg.SkipByTime(f) {
			continue
		}
		sErr := StreamFrom(ctx, st, seg, f, func(e Entry) bool {
			*out = append(*out, e)
			return limit <= 0 || len(*out) < limit
		})
		if sErr != nil {
			return false, sErr
		}
		if limit > 0 && len(*out) >= limit {
			return true, nil
		}
	}
	return false, nil
}
