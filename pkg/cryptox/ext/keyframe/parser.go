// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package keyframe 实现 shardseal.KeyframeIndexer：用 go-mp4 解析 MP4 容器，提取
// 关键帧（同步样本/I 帧）在文件中的绝对字节偏移，供 shardseal 视频关键帧分块规划。
// 仅遍历 moov→trak→mdia→minf→stbl 的 stco/co64、stsc、stsz、stss 四个 box（不读 mdat），
// 解析开销与文件尺寸无关。
package keyframe

import (
	"fmt"
	"io"
	"sort"

	"github.com/abema/go-mp4"
)

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
}

// handle 是 ReadBoxStructure 的 handler：trak → 开新表；stbl 子 box → 收集；容器 → 展开。
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
	// 只在 stbl 下收集样本表（path 末两段 [stbl, <box>]）；stbl 子 box 是叶子，无需 Expand。
	if c.cur != nil && len(p) >= 2 && p[len(p)-2] == mp4.StrToBoxType("stbl") {
		return nil, c.collectStbl(h, typ)
	}
	// 容器 box（moov/trak/mdia/minf/stbl 等）：必须 Expand 才会遍历子 box。
	_, err := h.Expand()
	return nil, err
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

// KeyframeOffsets 解析 MP4，返回关键帧（同步样本）在文件中的绝对字节偏移（升序）。
// 任意失败（缺 stss / 越界 / 截断 / 非 MP4）返回「已解析出的可用偏移 + err」，不 panic。
// 实现 shardseal.KeyframeIndexer。
func KeyframeOffsets(r io.ReaderAt, fileSize int64) ([]int64, error) {
	sr := io.NewSectionReader(r, 0, fileSize)
	c := &mp4Collector{}
	_, perr := mp4.ReadBoxStructure(sr, c.handle)
	// 解析失败不中断：可用部分照用（降级语义由 shardseal 决策）。
	if perr != nil && len(c.tables) == 0 {
		return nil, fmt.Errorf("keyframe: MP4 解析失败: %w", perr)
	}

	video := firstVideoTrack(c.tables)
	if video == nil {
		// 无 stss：可能截断或非视频（仅音频）。返回空 + 解析错误信息。
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
