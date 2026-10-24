// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// cacheWindow 是块（chunk）内**按 blocklet 聚合的缓存窗口**（2026-10-25 用户设计）。
//
// 设计要点：
//   - 窗口边界**恒落在 blocklet 边界上**（GCM 段不可分割 ⇒ 不切分 blocklet）；
//   - 聚合粒度目标 = stageWindowTargetBytes（默认 4MiB，= CDN 单次 Range 上限）；
//     单个 blocklet 超过该目标时**独占一窗**（不切分）；
//   - 窗口内容 = 该组 blocklet 的密文连续区间 [EncStart, EncEnd)（不含块头 salt/boot——
//     salt 由文件级盐本地构造，boot/index 只在整块解密时需要）；
//   - 每个窗口**独立下载 + 独立缓存**（缓存文件名含 chunk key + 窗口位点）；
//   - 读某个 blocklet ⇒ 只需其所属窗口；跨窗读 ⇒ 取所需窗口并集。
type cacheWindow struct {
	// Index 是窗口在块内的序号（0-based，与 planCacheWindows 输出顺序一致）。
	Index int
	// EncStart 是窗口密文起点（= 首个 blocklet 的 EncOffset）。
	EncStart int64
	// EncEnd 是窗口密文终点（= 末个 blocklet 的 EncOffset+EncSize，开区间）。
	EncEnd int64
	// Blets 是窗口包含的 blocklet 在 ChunkInfo.Blocklets 中的下标（升序）。
	Blets []int
}

// stageWindowTargetBytes 是**窗口聚合目标**（= CDN 单次 Range 上限 4MiB）：
// 把连续 blocklet 聚合到「不超过该值」为止，使「一次 Range = 一个窗口」。
const stageWindowTargetBytes = maxRangeSpan

// planCacheWindows 把块的 blocklet 按密文顺序聚合为窗口（纯函数，TDD 锁定）。
//
// 规则：
//  1. 只聚合 Used 且 EncSize>0 的 blocklet；
//  2. 当前窗加入下一个 blocklet 后总长 > targetBytes 时，收口并开新窗
//     （即单窗 ≤ targetBytes，除非单个 blocklet 本身 > targetBytes —— 此时独占一窗）；
//  3. 窗口按 EncOffset 升序、互不重叠、完整覆盖全部有效 blocklet。
func planCacheWindows(ci shardseal.ChunkInfo, targetBytes int64) []cacheWindow {
	if targetBytes <= 0 {
		targetBytes = stageWindowTargetBytes
	}
	var out []cacheWindow
	cur := cacheWindow{Index: 0, EncStart: -1}
	flush := func() {
		if len(cur.Blets) == 0 {
			return
		}
		out = append(out, cur)
		cur = cacheWindow{Index: len(out), EncStart: -1}
	}
	for i, bl := range ci.Blocklets {
		if !bl.Used || bl.EncSize <= 0 || bl.EncOffset < 0 {
			continue
		}
		if cur.EncStart < 0 {
			cur.EncStart = bl.EncOffset
		} else if bl.EncOffset+bl.EncSize-cur.EncStart > targetBytes {
			// 加入后超目标 ⇒ 先收口（当前窗已有内容），本 blocklet 起新窗。
			flush()
			cur.EncStart = bl.EncOffset
		}
		cur.Blets = append(cur.Blets, i)
		cur.EncEnd = bl.EncOffset + bl.EncSize
	}
	flush()
	return out
}

// windowsForRange 返回含 [offset,end) 相交 blocklet 的**窗口并集**（按 Index 升序、去重）。
// 返回值同时给出每个窗口内需要解密的 blocklet 下标集合（只解相交段，省 CPU）。
func windowsForRange(windows []cacheWindow, blocklets []shardseal.BlockletInfo, offset, end int64) ([]*cacheWindow, [][]int) {
	var wins []*cacheWindow
	var blets [][]int
	for i := range windows {
		var hits []int
		for _, bi := range windows[i].Blets {
			bl := blocklets[bi]
			if bl.Offset >= end || bl.Offset+bl.Size <= offset {
				continue
			}
			hits = append(hits, bi)
		}
		if len(hits) > 0 {
			wins = append(wins, &windows[i])
			blets = append(blets, hits)
		}
	}
	return wins, blets
}
