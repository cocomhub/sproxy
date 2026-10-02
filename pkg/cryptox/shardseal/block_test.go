// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"testing"
)

// 静态断言：RandomPlanner 满足 BlockPlanner 接口（编译期契约）。
var _ BlockPlanner = (*RandomPlanner)(nil)
var _ BlockletPlanner = (*FixedBlockletPlanner)(nil)

func TestRandomPlanner_TinyFileSingleChunk(t *testing.T) {
	t.Parallel()
	p := &RandomPlanner{Min: 64, Max: 256}
	blocks, err := p.Plan(50) // 50 < Min=64 → 单块收尾
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("50B 文件应切 1 块，got %d", len(blocks))
	}
	if blocks[0].Offset != 0 || blocks[0].Size != 50 {
		t.Errorf("单块不符：%+v", blocks[0])
	}
}

func TestRandomPlanner_MultiChunkTotalLength(t *testing.T) {
	t.Parallel()
	p := &RandomPlanner{Min: 64, Max: 128}
	blocks, err := p.Plan(1000)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var sum int64
	for _, b := range blocks {
		sum += b.Size
		if b.Size < 0 {
			t.Errorf("块大小非法: %+v", b)
		}
	}
	if sum != 1000 {
		t.Errorf("分块总长=%d want 1000", sum)
	}
	// 设为 Min==Max 时随机规划退化为定长（确定性）。
	p2 := &RandomPlanner{Min: 64, Max: 64}
	b2, err := p2.Plan(300)
	if err != nil {
		t.Fatalf("Plan2: %v", err)
	}
	want := 0
	for _, b := range b2 {
		_ = b
		want++
	}
	got := 0
	for range b2 {
		got++
	}
	if got != want {
		t.Errorf("内部计数不一致")
	}
	if len(b2) != 5 { // 300 = 64*4 + 44
		t.Errorf("块数=%d want 5", len(b2))
	}
}

func TestRandomPlanner_Invalid(t *testing.T) {
	t.Parallel()
	if _, err := (&RandomPlanner{Min: 0, Max: 0}).Plan(100); err == nil {
		t.Error("Min/Max 均 0 应报错")
	}
	if _, err := (&RandomPlanner{Min: 100, Max: 10}).Plan(100); err == nil {
		t.Error("Min>Max 应报错")
	}
	if _, err := (&RandomPlanner{Min: 10, Max: 100}).Plan(0); err == nil {
		t.Error("空文件应报错")
	}
}

func TestDefaultBlockPolicy(t *testing.T) {
	t.Parallel()
	p := DefaultBlockPolicy()
	if p.Mode != "random" {
		t.Errorf("mode=%q", p.Mode)
	}
	if p.Min != 1<<20 {
		t.Errorf("min=%d want 1MB", p.Min)
	}
	if p.Max != 200<<20 {
		t.Errorf("max=%d want 200MB", p.Max)
	}
	if p.BlockletMode != "fixed" {
		t.Errorf("blocklet_mode=%q, want fixed", p.BlockletMode)
	}
	if p.BlockletMin != 64<<10 {
		t.Errorf("blocklet_min=%d want 64KB", p.BlockletMin)
	}
	if p.BlockletMax != 4<<20 {
		t.Errorf("blocklet_max=%d want 4MB", p.BlockletMax)
	}
	if bp, lp := p.Planner(); bp == nil || lp == nil {
		t.Error("Planner() 返回的块/blocklet 规划器任一为 nil")
	}
}

func TestBlockPolicy_PlannerFactory(t *testing.T) {
	t.Parallel()
	p := BlockPolicy{Mode: "random", Min: 8, Max: 32, BlockletMin: 4, BlockletMax: 16}
	bp, lp := p.Planner()
	if bp == nil || lp == nil {
		t.Fatalf("Planner() 任一层为 nil：bp=%v lp=%v", bp, lp)
	}
	blocks, err := bp.Plan(100)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var sum int64
	for _, b := range blocks {
		sum += b.Size
		if b.Size < 0 {
			t.Errorf("块大小非法: %+v", b)
		}
	}
	if sum != 100 {
		t.Errorf("分块总长=%d want 100", sum)
	}
	// 首个块规划 blocklet：连续覆盖该块且不越出文件。
	bls, lerr := lp.PlanBlocklets(nil, 100, blocks[0].Offset, blocks[0].Size)
	if lerr != nil {
		t.Fatalf("PlanBlocklets: %v", lerr)
	}
	if len(bls) == 0 {
		t.Fatal("blocklet 序列为空")
	}
	if bls[0].Offset != blocks[0].Offset || bls[len(bls)-1].Offset+bls[len(bls)-1].Size != blocks[0].Offset+blocks[0].Size {
		t.Errorf("blocklet 未连续覆盖块 [%d,%d)：%+v", blocks[0].Offset, blocks[0].Offset+blocks[0].Size, bls)
	}
}

// TestBlockletPlanner_Fixed 验证定长 blocklet 规划器：多 blocklet 连续覆盖块、末块收尾、
// 每个 blocklet ≤Max 且 ≥1；非法参数（Min>Max / 空块 / 越界）fail-closed。
func TestBlockletPlanner_Fixed(t *testing.T) {
	t.Parallel()
	p := &FixedBlockletPlanner{Min: 8, Max: 32}
	bls, err := p.PlanBlocklets(nil, 1000, 100, 70) // 块 [100,170)，70B
	if err != nil {
		t.Fatalf("PlanBlocklets: %v", err)
	}
	// 70B / 32 = 2 块（64B）+ 收尾 6B → 3 块；每块 ≤32（收尾块 6<Min 允许）。
	var sum int64
	for i, bl := range bls {
		if bl.Offset != 100+sum {
			t.Errorf("bls[%d].Offset=%d，应为连续 %d", i, bl.Offset, 100+sum)
		}
		if bl.Size > 32 || bl.Size <= 0 {
			t.Errorf("bls[%d].Size=%d 越界 (1..32)", i, bl.Size)
		}
		sum += bl.Size
	}
	if sum != 70 {
		t.Errorf("blocklet 总长=%d want 70", sum)
	}
	if bls[0].Offset != 100 || bls[len(bls)-1].Offset+bls[len(bls)-1].Size != 170 {
		t.Errorf("blocklet 未覆盖块 [100,170)：%+v", bls)
	}

	// 非法/边界：
	if _, err := (&FixedBlockletPlanner{Min: 0, Max: 16}).PlanBlocklets(nil, 100, 0, 64); err == nil {
		t.Error("Min=0 应报错")
	}
	if _, err := (&FixedBlockletPlanner{Min: 16, Max: 8}).PlanBlocklets(nil, 100, 0, 64); err == nil {
		t.Error("Min>Max 应报错")
	}
	if _, err := p.PlanBlocklets(nil, 100, 0, 0); err == nil {
		t.Error("空块应报错")
	}
	if _, err := p.PlanBlocklets(nil, 64, 0, 100); err == nil {
		t.Error("块越出文件应报错")
	}
	if _, err := p.PlanBlocklets(nil, 100, -5, 10); err == nil {
		t.Error("负块偏移应报错")
	}
}
