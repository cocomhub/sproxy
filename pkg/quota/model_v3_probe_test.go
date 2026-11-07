// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import "testing"

// model_v3_probe_test.go 是 **配额模型 v3**（2026-10-14 用户裁定）的能力探针：
// 结构 = 卷（物理上限）→ 用户（每卷独立配额）→ 目录（目录级最大配额，最有价值能力）。
//
// 本文件先按**目标语义**写断言（TDD）：三层限额、分配账本（Σ子max ≤ 父max）、运行时改额、
// 释放沿父链传播。若现有实现不支持，则这些用例先红，随后在 pkg/quota 内补齐能力。
//
// 变异：任一层限额不生效 / 分配账本不校验 ⇒ 对应用例红。

// TestQuotaTree_ThreeLevelLimits 三层限额各能独立拦住（用量维度）。
func TestQuotaTree_ThreeLevelLimits(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)                                        // 卷：物理上限 1000
	owner := p.Scope("alice", 600)                            // 用户 alice 在本卷配额 600
	dir := owner.EnsureScope([]string{"user", "videos"}, 300) // 目录级配额 300

	res, err := dir.TryReserve(200) // 200 ≤ 目录300 / 用户600 / 卷1000
	if err != nil {
		t.Fatalf("首笔预留应通过: %v", err)
	}
	res.Commit(200)

	if _, err := dir.TryReserve(150); err == nil {
		t.Fatal("目录级限额应拦住（200+150 > 300）")
	}
	docs := owner.EnsureScope([]string{"user", "docs"}, 0)
	if _, err := docs.TryReserve(500); err == nil {
		t.Fatal("用户级限额应拦住（200+500 > 600，即使目录无限额）")
	}
	bob := p.Scope("bob", 400)
	if _, err := bob.TryReserve(900); err == nil {
		t.Fatal("卷级限额应拦住（卷 1000）")
	}
	// 用量可见性：三层各自可读。
	if got := dir.Usage(); got != 200 {
		t.Fatalf("目录用量 = %d want 200", got)
	}
	if got := owner.Usage(); got != 200 {
		t.Fatalf("用户用量 = %d want 200", got)
	}
	if got := p.Usage(); got != 200 {
		t.Fatalf("卷用量 = %d want 200", got)
	}
}

// TestQuotaTree_AllocationLedger 分配账本：Σ子 max ≤ 父 max（创建/改额时校验），且提供未分配量。
func TestQuotaTree_AllocationLedger(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)
	if _, err := p.TryAllocate(600); err != nil { // 给 alice 分配 600
		t.Fatalf("首次分配应通过: %v", err)
	}
	if _, err := p.TryAllocate(500); err == nil {
		t.Fatal("分配总额超过卷容量（600+500 > 1000）必须被拒")
	}
	if got := p.Unallocated(); got != 400 {
		t.Fatalf("未分配量 = %d want 400", got)
	}
	if _, err := p.TryAllocate(400); err != nil {
		t.Fatalf("用尽剩余分配应通过: %v", err)
	}
	if got := p.Unallocated(); got != 0 {
		t.Fatalf("未分配量 = %d want 0", got)
	}
}

// TestQuotaTree_ReleasePropagates 释放/调整沿父链传播（三层同步回落）。
func TestQuotaTree_ReleasePropagates(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)
	owner := p.Scope("alice", 600)
	dir := owner.EnsureScope([]string{"user", "videos"}, 300)
	res, err := dir.TryReserve(200)
	if err != nil {
		t.Fatalf("预留: %v", err)
	}
	res.Commit(200)
	dir.ReleaseUsage(200)
	for name, got := range map[string]int64{"dir": dir.Usage(), "owner": owner.Usage(), "vol": p.Usage()} {
		if got != 0 {
			t.Fatalf("%s 用量释放后 = %d want 0", name, got)
		}
	}
}

// TestQuotaTree_EnsureChildAllocation 配置路径：EnsureChild 同步分配账本，超额原子失败（不改状态）。
func TestQuotaTree_EnsureChildAllocation(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)
	alice, err := p.EnsureChild([]string{"alice"}, 600)
	if err != nil {
		t.Fatalf("分配 alice 600: %v", err)
	}
	if got := p.Unallocated(); got != 400 {
		t.Fatalf("未分配 = %d want 400", got)
	}
	if _, err := p.EnsureChild([]string{"bob"}, 400); err != nil {
		t.Fatalf("分配 bob 400: %v", err)
	}
	if got := p.Unallocated(); got != 0 {
		t.Fatalf("未分配 = %d want 0", got)
	}
	// 超额改额：必须原子失败（alice 上限与父分配都不变）。
	if _, err := p.EnsureChild([]string{"alice"}, 700); err == nil {
		t.Fatal("改额超额（已分配 1000 + 100 > 1000）必须失败")
	}
	if got := alice.MaxBytes(); got != 600 {
		t.Fatalf("失败后 alice 上限应保持 600, got %d", got)
	}
	if got := p.Allocated(); got != 1000 {
		t.Fatalf("失败后父分配应保持 1000, got %d", got)
	}
	// 缩额：归还差额。
	if _, err := p.EnsureChild([]string{"alice"}, 300); err != nil {
		t.Fatalf("缩额: %v", err)
	}
	if got := p.Unallocated(); got != 300 {
		t.Fatalf("缩额后未分配 = %d want 300", got)
	}
	// 目录级：从 alice 的额度里分配（Σ目录 ≤ alice 上限）。
	if _, err := alice.EnsureChild([]string{"user", "videos"}, 200); err != nil {
		t.Fatalf("目录配额 200（≤ alice 300）: %v", err)
	}
	// carve-out 语义（2026-10-18 定稿）：额度只记**直接父** ⇒ 中间层 user 无上限时不约束其子。
	// 该断言原期望"目录配额 > alice 上限即失败"，那需要逐级上溯（会双计，见 I1 定稿）。
	// 保留意图：**直接父**为有限额时，其子超额必须失败（把 videos 挂到有限额的 alice 下）。
	if _, err := alice.EnsureChild([]string{"videos"}, 400); err == nil {
		t.Fatal("直接子配额超过 alice 上限（300）必须失败")
	}
}
