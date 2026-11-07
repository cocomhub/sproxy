// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_window_test.go —— 窗口聚合纯函数锁定（2026-10-25 用户设计）。
//
// 断言：
//   - 窗口边界恒落在 blocklet 边界（不切分 blocklet）；
//   - 窗口 ≤ 目标（除非单 blocklet 超目标独占）；
//   - 完整覆盖全部有效 blocklet、按 EncOffset 升序、互不重叠；
//   - windowsForRange 取窗口并集且只解相交 blocklet。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// mkBlocklets 构造 blocklet 序列（off/size 明文 + encOff/encSize 密文；EncSize 与明文
// size 的关系 = 段头 12B nonce + 明文 + 16B tag，即 EncSize = size + 28）。
func mkBlocklets(plainOffsets []int64, plainSizes []int64, encBase int64) []shardseal.BlockletInfo {
	out := make([]shardseal.BlockletInfo, len(plainOffsets))
	enc := encBase
	for i := range plainOffsets {
		out[i] = shardseal.BlockletInfo{
			Offset: plainOffsets[i], Size: plainSizes[i],
			EncOffset: enc, EncSize: plainSizes[i] + 28,
			Used: true,
		}
		enc += plainSizes[i] + 28
	}
	return out
}

// TestPlanCacheWindows_AggregatesWithinTarget：多个小 blocklet 聚合进 ≤ target 窗口。
func TestPlanCacheWindows_AggregatesWithinTarget(t *testing.T) {
	t.Parallel()
	// 8 个 1MiB blocklet（EncSize 1MiB+28）；target 4MiB ⇒ 每窗装到「再加就超」为止：
	// 3+3+2（3×（1MiB+28）= 3MiB+84 ≤ 4MiB；4 个 = 4MiB+112 > 4MiB）。
	bls := mkBlocklets(
		[]int64{0, 1 << 20, 2 << 20, 3 << 20, 4 << 20, 5 << 20, 6 << 20, 7 << 20},
		[]int64{1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20},
		300,
	)
	wins := planCacheWindows(shardseal.ChunkInfo{Blocklets: bls}, 4<<20)
	if len(wins) != 3 {
		t.Fatalf("窗口数=%d，应为 3", len(wins))
	}
	wantCounts := []int{3, 3, 2}
	for i, w := range wins {
		if len(w.Blets) != wantCounts[i] {
			t.Fatalf("窗%d blocklet 数=%d，应为 %d", i, len(w.Blets), wantCounts[i])
		}
		if w.EncEnd-w.EncStart > 4<<20 && len(w.Blets) > 1 {
			t.Fatalf("窗%d 长度 %d 超 target 且含多个 blocklet", i, w.EncEnd-w.EncStart)
		}
		if i > 0 && wins[i-1].EncEnd != w.EncStart {
			t.Fatalf("窗口间密文不连续：prev end=%d cur start=%d", wins[i-1].EncEnd, w.EncStart)
		}
	}
	total := 0
	for _, w := range wins {
		total += len(w.Blets)
	}
	if total != 8 {
		t.Fatalf("覆盖 blocklet 总数=%d，应为 8", total)
	}
	last := wins[len(wins)-1]
	if last.EncEnd != bls[7].EncOffset+bls[7].EncSize {
		t.Fatalf("末窗终点=%d，应为 %d", last.EncEnd, bls[7].EncOffset+bls[7].EncSize)
	}
}

// TestPlanCacheWindows_SingleBletExceedsTarget：单个大 blocklet 独占一窗（不切分）。
func TestPlanCacheWindows_SingleBletExceedsTarget(t *testing.T) {
	t.Parallel()
	// 8MiB blocklet（> 4MiB target）⇒ 独占一窗。
	bls := mkBlocklets([]int64{0, 8 << 20}, []int64{8 << 20, 8 << 20}, 300)
	wins := planCacheWindows(shardseal.ChunkInfo{Blocklets: bls}, 4<<20)
	if len(wins) != 2 {
		t.Fatalf("窗口数=%d，应为 2（每个 8MiB blocklet 独占一窗）", len(wins))
	}
	for i, w := range wins {
		if len(w.Blets) != 1 {
			t.Fatalf("窗%d 应含 1 个 blocklet", i)
		}
		bl := bls[w.Blets[0]]
		if w.EncStart != bl.EncOffset || w.EncEnd != bl.EncOffset+bl.EncSize {
			t.Fatalf("窗%d 区间 [%d,%d) 应等于 blocklet 密文 [%d,%d)", i, w.EncStart, w.EncEnd, bl.EncOffset, bl.EncOffset+bl.EncSize)
		}
	}
}

// TestPlanCacheWindows_SkipsUnused：跳过 Padding（Used=false）blocklet，窗口不含它们。
func TestPlanCacheWindows_SkipsUnused(t *testing.T) {
	t.Parallel()
	bls := mkBlocklets([]int64{0, 1 << 20, 2 << 20}, []int64{1 << 20, 1 << 20, 1 << 20}, 300)
	bls[1].Used = false // 中间 padding
	wins := planCacheWindows(shardseal.ChunkInfo{Blocklets: bls}, 4<<20)
	if len(wins) != 1 {
		t.Fatalf("窗口数=%d，应为 1（跳过 padding）", len(wins))
	}
	if len(wins[0].Blets) != 2 {
		t.Fatalf("窗口应含 2 个有效 blocklet（跳过 padding）")
	}
	if wins[0].Blets[0] != 0 || wins[0].Blets[1] != 2 {
		t.Fatalf("窗口 blocklet 下标=%v，应为 [0 2]", wins[0].Blets)
	}
}

// TestWindowsForRange_UnionAcrossWindows：跨窗读取窗口并集 + 只解相交 blocklet。
func TestWindowsForRange_UnionAcrossWindows(t *testing.T) {
	t.Parallel()
	// 6 个 1MiB blocklet（EncSize 1MiB+28）；target 2MiB ⇒ 每窗 1 个 blocklet
	// （2×（1MiB+28）= 2MiB+56 > 2MiB）⇒ 6 窗，便于构造跨窗场景。
	bls := mkBlocklets(
		[]int64{0, 1 << 20, 2 << 20, 3 << 20, 4 << 20, 5 << 20},
		[]int64{1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 1 << 20},
		300,
	)
	wins := planCacheWindows(shardseal.ChunkInfo{Blocklets: bls}, 2<<20)
	if len(wins) != 6 {
		t.Fatalf("前置：窗口数=%d，应为 6", len(wins))
	}
	// 读明文 [1.2MiB, 3.6MiB)：相交 blocklet 1、2、3 ⇒ 窗口 1、2、3。
	gotWins, gotBlets := windowsForRange(wins, bls, 1<<20+(1<<20/5), 3<<20+(3<<20/5))
	if len(gotWins) != 3 {
		t.Fatalf("窗口数=%d，应为 3（跨窗）", len(gotWins))
	}
	wantB := []int{1, 2, 3}
	for i := range wantB {
		if gotBlets[i][0] != wantB[i] {
			t.Fatalf("相交 blocklet 下标=%v，应为 %v", gotBlets, wantB)
		}
	}
	// 读明文 [0, 1MiB)：只窗口 0。
	wins2, blets2 := windowsForRange(wins, bls, 0, 1<<20)
	if len(wins2) != 1 || wins2[0].Index != 0 {
		t.Fatalf("单窗读应只命中窗口 0（got %d）", len(wins2))
	}
	if len(blets2) != 1 || blets2[0][0] != 0 {
		t.Fatalf("相交 blocklet 应为 [0]")
	}
}
