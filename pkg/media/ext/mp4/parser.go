// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package keyframe 实现 shardseal.KeyframeIndexer：用 go-mp4 解析 MP4 容器，提取
// 关键帧（同步样本/I 帧）在文件中的绝对字节偏移，供 shardseal 视频关键帧分块规划。
// 仅遍历 moov→trak→mdia→minf→stbl 的 stco/co64、stsc、stsz、stss 四个 box（不读 mdat），
// 解析开销与文件尺寸无关。
package keyframe

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync/atomic"

	"github.com/abema/go-mp4"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// ErrFragmentedMP4 是哨兵错误：检测到 fMP4（fragmented MP4，moof/mvex，无全局 stss）——
// go-mp4 无法定位关键帧，调用方（shardseal）回落 fixed。此错误**只**标记 fMP4 降级
// （区别于普通非视频/截断），供日志统计与后续「是否切 mp4ff」决策。
var ErrFragmentedMP4 = fmt.Errorf("keyframe: fMP4 不支持（无全局 stss，关键帧定位降级）")

// onFragmentedMP4 是 fMP4 检测统计 hook（atomic.Pointer 保证读写无数据竞争——装配期
// SetOnFragmentedMP4 与运行期 notify 可跨 goroutine 并发，显式内存屏障）。fMP4 出现频次
// 是「是否切 mp4ff」的真实场景决策依据（metric 可聚合监控，优于翻日志）。
var onFragmentedMP4 atomic.Pointer[func()]

// SetOnFragmentedMP4 注入 fMP4 统计 hook（装配层调用 server.Metrics.RecordKeyframeFragmented；
// nil = 清除，不记录，零回归）。
func SetOnFragmentedMP4(fn func()) {
	if fn == nil {
		onFragmentedMP4.Store(nil)
		return
	}
	onFragmentedMP4.Store(&fn)
}

// notifyFragmentedMP4 触发 fMP4 统计 hook（nil 安全）。
func notifyFragmentedMP4() {
	if p := onFragmentedMP4.Load(); p != nil {
		(*p)()
	}
}

// sampleTable 是单个轨的样本定位表（stbl 子 box）。
type sampleTable struct {
	chunkOffset64 []uint64 // stco/co64 归一为 uint64（chunk 绝对偏移，1-based 下标）
	sc            []mp4.StscEntry
	sizes         []uint32 // 每样本大小（stsz；SampleSize>0 时全等）
	sampleSize    uint32   // stsz.SampleSize（>0 = 定长样本）
	sync          []uint32 // stss.SampleNumber（1-based 同步样本序号）
}

// mp4Collector 是 ReadBoxStructure 的遍历状态：按 box 路径收集各轨样本表。
// （S107/gocognit 收敛：handler 闭包状态与分发逻辑归组为类型方法。）
type mp4Collector struct {
	tables []*sampleTable
	cur    *sampleTable
	// fragmented 标记该 MP4 是否含 fMP4 特征（moof/mvex box——sample 表分散在
	// fragment 而非全局 stss）。fMP4 无全局 stss，go-mp4 无法定位关键帧 → 降级 fixed。
	fragmented bool
}

// handle 是 ReadBoxStructure 的 handler：trak → 开新表；stbl 子 box → 收集；已知容器
// box（moov/mdia/minf/stbl/udta 等）→ 展开子 box；**mdat 等媒体数据 box 绝不 Expand**
// （评审实测 4.6GB 文件：Expand 会把整个 mdat 读进内存——heap 14GB + 32s，大文件内存爆炸）。
func (c *mp4Collector) handle(h *mp4.ReadHandle) (any, error) {
	typ := h.BoxInfo.Type
	p := h.Path
	// 新轨：path 末两段 [moov, trak]（不依赖 ftyp 前置）。
	if len(p) >= 2 && p[len(p)-2] == mp4.StrToBoxType("moov") && p[len(p)-1] == mp4.StrToBoxType("trak") {
		c.cur = &sampleTable{}
		c.tables = append(c.tables, c.cur)
		_, err := h.Expand()
		return nil, err
	}
	// fMP4 特征：moof（fragment 容器）或 mvex（fragment 声明）出现在任意层级。
	if typ == mp4.StrToBoxType("moof") || typ == mp4.StrToBoxType("mvex") {
		c.fragmented = true
		return nil, nil
	}
	// 只在 stbl 下收集样本表（path 末两段 [stbl, <box>]）；stbl 子 box 是叶子，无需 Expand。
	if c.cur != nil && len(p) >= 2 && p[len(p)-2] == mp4.StrToBoxType("stbl") {
		return nil, c.collectStbl(h, typ)
	}
	// 仅展开已知容器 box（元数据树内部）；mdat/free/wide 等数据或填充 box 跳过——
	// mdat 可占文件 99% 体积，Expand 会 UnmarshalAny 读入内存（大文件内存爆炸，评审实测）。
	if typ == mp4.StrToBoxType("moov") || typ == mp4.StrToBoxType("mdia") ||
		typ == mp4.StrToBoxType("minf") || typ == mp4.StrToBoxType("stbl") ||
		typ == mp4.StrToBoxType("udta") || typ == mp4.StrToBoxType("trak") {
		_, err := h.Expand()
		return nil, err
	}
	return nil, nil // mdat/free 等非容器 box：不展开（不读载荷）
}

// collectStbl 从 stbl 子 box 读取样本定位表。
func (c *mp4Collector) collectStbl(h *mp4.ReadHandle, typ mp4.BoxType) error {
	box, _, err := h.ReadPayload()
	if err != nil {
		return err
	}
	switch {
	case typ == mp4.StrToBoxType("stco") || typ == mp4.StrToBoxType("co64"):
		c.cur.chunkOffset64 = append(c.cur.chunkOffset64, chunkOffsets64(box)...)
	case typ == mp4.StrToBoxType("stsc"):
		if s, ok := box.(*mp4.Stsc); ok {
			c.cur.sc = append(c.cur.sc, s.Entries...)
		}
	case typ == mp4.StrToBoxType("stsz"):
		if s, ok := box.(*mp4.Stsz); ok {
			c.cur.sampleSize = s.SampleSize
			c.cur.sizes = append(c.cur.sizes, s.EntrySize...)
		}
	case typ == mp4.StrToBoxType("stss"):
		if s, ok := box.(*mp4.Stss); ok {
			c.cur.sync = append(c.cur.sync, s.SampleNumber...)
		}
	}
	return nil
}

// Indexer 是 shardseal.KeyframeIndexer 的装配实例（go-mp4 解析器）。
type Indexer struct{}

// KeyframeOffsets 实现 shardseal.KeyframeIndexer（req 单一模式，2026-10-04）：解析 MP4，
// 返回关键帧（同步样本）在文件中的绝对字节偏移（升序）。go-mp4 是内存 moov 解析（不依赖
// 文件路径），req.Reader 即可；req.Path 忽略（无 stdin 局限）。任意失败（缺 stss / 越界 /
// 截断 / 非 MP4）返回「已解析出的可用偏移 + err」，不 panic（降级由 shardseal 决策）。
func (Indexer) KeyframeOffsets(req shardseal.KeyframeRequest) ([]int64, error) {
	return KeyframeOffsets(req.Reader, req.Size)
}

// KeyframeOffsets 解析 MP4，返回关键帧（同步样本）在文件中的绝对字节偏移（升序）。
// 任意失败（缺 stss / 越界 / 截断 / 非 MP4）返回「已解析出的可用偏移 + err」，不 panic。
// 实现 shardseal.KeyframeIndexer。
//
// **panic 兜底（审查 I3 修复，2026-10-04）**：上传文件是用户不可信输入，go-mp4 对恶意
// 构造的 MP4（畸形 box 长度/类型）存在 panic 可能（第三方库契约未保证不 panic）——若不
// recover 会经 PlanBlocklets → encryptWriteChunks 逃逸到整个写路径乃至服务器进程。此处
// recover 转 error（→ 调用方固定降级），兑现「解析不可信输入不 panic」硬约束。
func KeyframeOffsets(r io.ReaderAt, fileSize int64) (offs []int64, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			offs = nil
			err = fmt.Errorf("keyframe: 解析 panic 已隔离（不可信输入）: %v", rec)
		}
	}()
	// **大文件性能优化（pprof 实测 2026-10-04）**：整文件 SectionReader 解析 4.6GB MP4
	// 需 22.6s——pprof 显示 90%+ 在 syscall（go-mp4 反射 unmarshal 逐字段小读触发
	// 每次 Pread/Seek）。优化：先扫描 box 结构定位 moov 偏移（只读几个 box 头 + seek，
	// 毫秒级），把 moov（4.6GB 文件实测 12.7MB）一次 ReadAt 进内存，内存解析——24958 条
	// stss 从 22.6s 降到 ~5ms（实测）。mdat 只跳过不读（不占内存）。
	moovOff, moovSize, isFrag := locateMoov(r, fileSize)
	if moovOff < 0 {
		// 无 moov（非 MP4 / 截断 / fMP4 只有 moof）：按原有逻辑尝试整文件遍历兜底。
		return keyframeOffsetsFull(r, fileSize)
	}
	// fMP4 特征（moof/mvex）已在 locateMoov 顺带检测。
	if isFrag {
		slog.Warn("keyframe: 检测到 fMP4（moof/mvex，无全局 stss），关键帧定位降级 fixed——记录以评估 mp4ff 切换", "file_size", fileSize)
		notifyFragmentedMP4() // metric 统计（装配层注入；atomic 读，nil 安全）
		return nil, ErrFragmentedMP4
	}
	// 读 moov 进内存（一次大 ReadAt，消除 syscall 风暴）。
	moov := make([]byte, moovSize)
	if _, rerr := r.ReadAt(moov, moovOff); rerr != nil && rerr != io.EOF {
		return nil, fmt.Errorf("keyframe: 读 moov 失败: %w", rerr)
	}
	c := &mp4Collector{}
	_, perr := mp4.ReadBoxStructure(bytes.NewReader(moov), c.handle)
	if perr != nil && len(c.tables) == 0 {
		return nil, fmt.Errorf("keyframe: MP4 解析失败: %w", perr)
	}
	video := firstVideoTrack(c.tables)
	if video == nil {
		if perr != nil {
			return nil, fmt.Errorf("keyframe: 未找到关键帧表且解析失败: %w", perr)
		}
		return nil, fmt.Errorf("keyframe: MP4 无同步样本表（stss 缺失，非视频或不可解析）")
	}
	offs, oerr := video.keyframeOffsets()
	if oerr != nil {
		return offs, fmt.Errorf("keyframe: %w", oerr)
	}
	return offs, nil
}

// keyframeOffsetsFull 是原整文件遍历实现（无 moov 时的兜底：非 MP4/截断/fMP4 特征不明）。
func keyframeOffsetsFull(r io.ReaderAt, fileSize int64) ([]int64, error) {
	sr := io.NewSectionReader(r, 0, fileSize)
	c := &mp4Collector{}
	_, perr := mp4.ReadBoxStructure(sr, c.handle)
	if perr != nil && len(c.tables) == 0 {
		return nil, fmt.Errorf("keyframe: MP4 解析失败: %w", perr)
	}
	video := firstVideoTrack(c.tables)
	if video == nil {
		if c.fragmented {
			slog.Warn("keyframe: 检测到 fMP4（moof/mvex，无全局 stss），关键帧定位降级 fixed——记录以评估 mp4ff 切换", "file_size", fileSize)
			notifyFragmentedMP4()
			return nil, ErrFragmentedMP4
		}
		if perr != nil {
			return nil, fmt.Errorf("keyframe: 未找到关键帧表且解析失败: %w", perr)
		}
		return nil, fmt.Errorf("keyframe: MP4 无同步样本表（stss 缺失，非视频或不可解析）")
	}
	offs, oerr := video.keyframeOffsets()
	if oerr != nil {
		return offs, fmt.Errorf("keyframe: %w", oerr)
	}
	return offs, nil
}

// locateMoov 扫描顶层 box 结构定位 moov 偏移/大小，顺带检测 fMP4（moof/mvex）。
// 只读 box 头（8B/个）+ 按 size 跳转（mdat 等大 box 跳过不读），毫秒级。
// 返回 (moovOff, moovSize, isFragmented)；无 moov 返回 (-1, 0, false)。
func locateMoov(r io.ReaderAt, fileSize int64) (moovOff, moovSize int64, isFragmented bool) {
	const maxScanBoxes = 64
	off := int64(0)
	for range maxScanBoxes {
		size, typ, ok := readMP4BoxHeader(r, off)
		if !ok || (size < 8 && size != 1) {
			// size==1 是 64 位扩展合法标记（4.6GB 真实文件 mdat 用）；其余 <8 非法。
			return -1, 0, false
		}
		if typ == "moov" {
			return off, int64(size), isFragmented
		}
		if typ == "moof" || typ == "mvex" {
			isFragmented = true
		}
		next, stop, ok := advanceMP4Box(r, off, size)
		if !ok || stop {
			return -1, 0, isFragmented
		}
		off = next
	}
	return -1, 0, isFragmented
}

// readMP4BoxHeader 读 MP4 box 头（[4B size][4B type]）。
func readMP4BoxHeader(r io.ReaderAt, off int64) (size uint64, typ string, ok bool) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return 0, "", false
	}
	size = uint64(hdr[0])<<24 | uint64(hdr[1])<<16 | uint64(hdr[2])<<8 | uint64(hdr[3])
	return size, string(hdr[4:8]), true
}

// advanceMP4Box 计算下一 box 偏移：size==0 延伸文件尾（stop）；size==1 是 64 位扩展
// （读取 8B 扩展字段作为真实 size——4.6GB 真实文件 mdat 用 64 位 size，实测定位 moov
// 需支持；debug 实证 ftyp→mdat(64bit)→moov）。否则 next = off+size。
func advanceMP4Box(r io.ReaderAt, off int64, size uint64) (nextOff int64, stop bool, ok bool) {
	if size == 0 {
		return 0, true, false
	}
	if size == 1 {
		var ext [8]byte
		if _, err := r.ReadAt(ext[:], off+8); err != nil {
			return 0, false, false
		}
		size = uint64(ext[0])<<56 | uint64(ext[1])<<48 | uint64(ext[2])<<40 | uint64(ext[3])<<32 |
			uint64(ext[4])<<24 | uint64(ext[5])<<16 | uint64(ext[6])<<8 | uint64(ext[7])
	}
	return off + int64(size), false, true
}

// firstVideoTrack 返回第一个含 stss 的轨（主视频轨；音频轨无 stss）。
func firstVideoTrack(tables []*sampleTable) *sampleTable {
	for _, t := range tables {
		if len(t.sync) > 0 {
			return t
		}
	}
	return nil
}

// chunkOffsets64 把 stco/co64 载荷归一为 uint64 切片。
func chunkOffsets64(box mp4.IBox) []uint64 {
	switch b := box.(type) {
	case *mp4.Stco:
		out := make([]uint64, len(b.ChunkOffset))
		for i, v := range b.ChunkOffset {
			out[i] = uint64(v)
		}
		return out
	case *mp4.Co64:
		return b.ChunkOffset
	default:
		return nil
	}
}

// keyframeOffsets 由样本表计算同步样本的文件绝对偏移。
// 算法：stss 给同步样本序号（1-based）；stsc 给 sample→chunk 映射；stco/co64 给 chunk
// 绝对偏移；stsz 给样本大小。任一样本定位失败即停止并返回已解析部分 + err（视频截断时
// 可用部分仍可播放，降级由 shardseal 决策）。
func (t *sampleTable) keyframeOffsets() ([]int64, error) {
	if len(t.chunkOffset64) == 0 || len(t.sc) == 0 {
		return nil, fmt.Errorf("样本定位表缺失（stco/stsc）")
	}
	if len(t.sizes) == 0 && t.sampleSize == 0 {
		return nil, fmt.Errorf("样本大小表缺失（stsz）")
	}
	out := make([]int64, 0, len(t.sync))
	for _, num := range t.sync {
		if num < 1 {
			continue
		}
		off, ok := t.sampleOffset(num - 1) // stss 为 1-based，转 0-based
		if !ok {
			// 越界样本（截断）：停止，保留已解析部分。
			return out, fmt.Errorf("同步样本 %d 越出样本表（视频截断）", num)
		}
		out = append(out, off)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// sampleOffset 返回 0-based 样本序号的绝对字节偏移。
// stsc entries 定义连续 chunk 段内每 chunk 的样本数；逐段累加定位样本所在 chunk 与
// chunk 内序号，再叠加 stsz 前序样本大小。
func (t *sampleTable) sampleOffset(s uint32) (int64, bool) {
	chunkAbs, chunkStartSample, ok := t.locateChunk(s)
	if !ok {
		return 0, false
	}
	off := t.chunkOffset64[chunkAbs]
	for j := chunkStartSample; j < s; j++ {
		sz, ok := t.sizeOf(j)
		if !ok {
			return 0, false
		}
		off += uint64(sz)
	}
	return int64(off), true
}

// locateChunk 返回 0-based 样本所在 chunk（0-based）与该 chunk 起始样本序号。
// 遍历 stsc 段累加定位；样本越出全表返回 ok=false。
func (t *sampleTable) locateChunk(s uint32) (chunkAbs uint32, chunkStartSample uint32, ok bool) {
	var sampleStart uint32 // 当前 stsc 段起始样本（0-based）
	for i, e := range t.sc {
		nextFirst := t.nextFirstChunk(i)
		if nextFirst < e.FirstChunk || e.FirstChunk < 1 || e.SamplesPerChunk == 0 {
			return 0, 0, false
		}
		chunkCount := nextFirst - e.FirstChunk
		segSamples := chunkCount * e.SamplesPerChunk
		if s >= sampleStart && s < sampleStart+segSamples {
			within := s - sampleStart
			chunkIdx := within / e.SamplesPerChunk // 段内 chunk（0-based）
			abs := e.FirstChunk - 1 + chunkIdx
			if int(abs) >= len(t.chunkOffset64) {
				return 0, 0, false
			}
			return abs, sampleStart + chunkIdx*e.SamplesPerChunk, true
		}
		sampleStart += segSamples
	}
	return 0, 0, false
}

// nextFirstChunk 返回第 i 个 stsc 段的下一段起始 chunk（最后一段 = chunk 总数 + 1）。
func (t *sampleTable) nextFirstChunk(i int) uint32 {
	if i+1 < len(t.sc) {
		return t.sc[i+1].FirstChunk
	}
	return uint32(len(t.chunkOffset64)) + 1
}

// sizeOf 返回 0-based 样本序号的大小（stsz 定长或逐样本）。
func (t *sampleTable) sizeOf(s uint32) (uint32, bool) {
	if t.sampleSize > 0 {
		return t.sampleSize, true
	}
	if int(s) >= len(t.sizes) {
		return 0, false
	}
	return t.sizes[s], true
}
