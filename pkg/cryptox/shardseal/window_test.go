// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

// window_test.go —— 写侧窗口规划锁定（2026-10-25 用户设计）。
//
// 断言：
//   - 首窗起点 = blSaltOff（含 salt）；末窗终点 = blobLen（连续覆盖）；
//   - 首位相连（每窗起点 = 上一窗终点，无空洞）；
//   - 窗口终点落在 blocklet 边界（不切分 blocklet；单大 blocklet 独占）；
//   - 每窗长度 ≤ 目标（除非独占）；
//   - FillWindowChecksums / VerifyWindowChecksum 往返一致；篡改检测 fail-closed。

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// mkTestWindows 构造 blocklet 索引（encBase 起；EncSize = 明文+28 段开销）。
func mkTestBlets(offsets []int64, encBase int64) []BlockletInfo {
	out := make([]BlockletInfo, len(offsets))
	enc := encBase
	for i, off := range offsets {
		size := int64(1 << 20) // 1MiB 明文
		out[i] = BlockletInfo{
			Offset: off, Size: size, EncOffset: enc, EncSize: size + 28,
			Used: true, Type: byte(BlockletTypeData),
		}
		enc += size + 28
	}
	return out
}

// TestPlanChunkWindows_FirstFromSaltLastAtEnd：首窗含 salt、末窗到 blob 尾。
func TestPlanChunkWindows_FirstFromSaltLastAtEnd(t *testing.T) {
	t.Parallel()
	// blobLen = blSaltOff + 4 个 1MiB blocklet + 尾 index 段。
	bl := mkTestBlets([]int64{0, 1 << 20, 2 << 20, 3 << 20}, int64(blListOff)+40) // bootEncSize 之后
	blobLen := int64(blSaltOff) + 40                                              // salt + boot
	for _, b := range bl {
		blobLen = b.EncOffset + b.EncSize
	}
	blobLen += 300 // index 段
	wins := PlanChunkWindows(blobLen, bl, 4<<20)
	if len(wins) == 0 {
		t.Fatal("应有窗口")
	}
	if wins[0].EncOffset != int64(blSaltOff) {
		t.Fatalf("首窗起点=%d，应为 %d（含 salt）", wins[0].EncOffset, int64(blSaltOff))
	}
	last := wins[len(wins)-1]
	if last.EncOffset+last.EncSize != blobLen {
		t.Fatalf("末窗终点=%d，应为 %d（连续到块尾）", last.EncOffset+last.EncSize, blobLen)
	}
	// 首位相连。
	for i := 1; i < len(wins); i++ {
		if wins[i].EncOffset != wins[i-1].EncOffset+wins[i-1].EncSize {
			t.Fatalf("窗口 %d 起点=%d，应=%d（首位相连）", i, wins[i].EncOffset, wins[i-1].EncOffset+wins[i-1].EncSize)
		}
	}
}

// TestPlanChunkWindows_AggregatesAndSingleExceeds：聚合 + 单大 blocklet 独占。
func TestPlanChunkWindows_AggregatesAndSingleExceeds(t *testing.T) {
	t.Parallel()
	// 5 个 1MiB blocklet（EncSize 1MiB+28）；target 3MiB ⇒ 每窗 ≤3 个。
	bl := mkTestBlets([]int64{0, 1 << 20, 2 << 20, 3 << 20, 4 << 20}, int64(blListOff)+40)
	blobLen := int64(blSaltOff) + 40
	for _, b := range bl {
		blobLen = b.EncOffset + b.EncSize
	}
	blobLen += 200
	wins := PlanChunkWindows(blobLen, bl, 3<<20)
	if len(wins) < 2 {
		t.Fatalf("应有 ≥2 窗，got %d", len(wins))
	}
	for _, w := range wins {
		if w.EncSize > 3<<20+28 {
			t.Fatalf("窗 %d 长 %d 超目标（应聚合 ≤3MiB）", w.Index, w.EncSize)
		}
	}
	// 单大 blocklet（8MiB > 3MiB target）⇒ 独占一窗。
	big := mkTestBlets([]int64{0}, int64(blListOff)+40)
	big[0].Size = 8 << 20
	big[0].EncSize = 8<<20 + 28
	blobLen2 := big[0].EncOffset + big[0].EncSize
	wins2 := PlanChunkWindows(blobLen2, big, 3<<20)
	if len(wins2) != 1 {
		t.Fatalf("单大 blocklet 应独占 1 窗，got %d", len(wins2))
	}
	// 独占窗 = [blSaltOff, blobLen)（含块头 salt + 该 blocklet + index 段）。
	if wins2[0].EncOffset != int64(blSaltOff) || wins2[0].EncOffset+wins2[0].EncSize != blobLen2 {
		t.Fatalf("独占窗 [%d,%d) 应 = [%d,%d)", wins2[0].EncOffset, wins2[0].EncOffset+wins2[0].EncSize, int64(blSaltOff), blobLen2)
	}
}

// TestFillVerifyWindowChecksums：写侧填 checksum，读侧校验一致；篡改 fail-closed。
func TestFillVerifyWindowChecksums(t *testing.T) {
	t.Parallel()
	bl := mkTestBlets([]int64{0, 1 << 20, 2 << 20, 3 << 20}, int64(blListOff)+40)
	blobLen := int64(blSaltOff) + 40
	for _, b := range bl {
		blobLen = b.EncOffset + b.EncSize
	}
	blobLen += 300
	wins := PlanChunkWindows(blobLen, bl, 4<<20)
	enc := make([]byte, blobLen)
	rand.Read(enc) //nolint:gosec // 测试
	if err := FillWindowChecksums(wins, enc); err != nil {
		t.Fatalf("FillWindowChecksums: %v", err)
	}
	for _, w := range wins {
		if w.Checksum == "" {
			t.Fatalf("窗 %d checksum 未填", w.Index)
		}
		if err := VerifyWindowChecksum(w, enc[w.EncOffset:w.EncOffset+w.EncSize]); err != nil {
			t.Fatalf("VerifyWindowChecksum(窗 %d): %v", w.Index, err)
		}
	}
	// 篡改：改窗口中间字节 ⇒ 校验失败。
	mut := bytes.Clone(enc[wins[0].EncOffset : wins[0].EncOffset+wins[0].EncSize])
	mut[len(mut)/2] ^= 0xFF
	if err := VerifyWindowChecksum(wins[0], mut); err == nil {
		t.Fatal("篡改窗口应校验失败（fail-closed）")
	}
	// 无 checksum（旧 meta）⇒ 跳过校验（零回归）。
	if err := VerifyWindowChecksum(WindowInfo{Index: 0, EncOffset: 0, EncSize: 100}, enc[:100]); err != nil {
		t.Fatalf("旧格式（无 checksum）应跳过校验: %v", err)
	}
}
