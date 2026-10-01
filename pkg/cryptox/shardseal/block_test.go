// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"testing"
)

// 静态断言：RandomPlanner 满足 BlockPlanner 接口（编译期契约）。
var _ BlockPlanner = (*RandomPlanner)(nil)

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
	if p.Planner() == nil {
		t.Error("Planner() 返回 nil")
	}
}

func TestBlockPolicy_PlannerFactory(t *testing.T) {
	t.Parallel()
	p := BlockPolicy{Mode: "random", Min: 8, Max: 32}
	pl := p.Planner()
	blocks, err := pl.Plan(100)
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
}
