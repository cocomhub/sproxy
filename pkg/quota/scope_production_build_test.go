// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// scope_production_build_test.go 钉住「**生产装配路径与 EnsureChild 共用同一记账实现**」
// （独立复审 B-P2-4 根因修复，2026-10-18 + 用户裁定启用 I1 强制）。
//
// 缺陷：生产建树走 `Pool.Scope`/`Scope.Mount`/`EnsureScope`（**只建节点、不记分配账**）⇒
// `Allocated()/Unallocated()` 恒 0/失真、I1（Σ 子树上限 ≤ 父可用上限）在这些节点上**无强制点**；
// 与 `EnsureChild` 形成「两套建树方式」（用户明示要避免的"多套同时存在"）。
// 修法：三者共用 `setChildLimit`（唯一记账实现）⇒ 记账 + I1 强制 + 超额**原子失败**（返回 nil）。

import "testing"

func TestScopeProductionBuild_AccountsAndEnforcesI1(t *testing.T) {
	t.Parallel()
	root := NewPool(1000)
	// ① Pool.Scope（生产：/tenant/<owner>）必须记账（未修必红：旧实现 allocated 恒 0）。
	s := root.Scope("/tenant/alice", 600)
	if s == nil {
		t.Fatal("合法分配（600 ≤ 1000）不得被拒")
	}
	if got, want := root.Allocated(), int64(600); got != want {
		t.Fatalf("root.Allocated()=%d want %d（生产建树必须记账；未修必红：恒 0）", got, want)
	}
	if got, want := root.Unallocated(), int64(400); got != want {
		t.Fatalf("root.Unallocated()=%d want %d（可见性）", got, want)
	}
	// ② 功能桶挂载（上限 0 = 不限制）不得改动分配账本。
	if b := s.Mount("user", 0); b == nil {
		t.Fatal("挂载功能桶（0 上限）不得被拒")
	}
	if got, want := root.Allocated(), int64(600); got != want {
		t.Fatalf("挂载 0 上限桶后 root.Allocated()=%d want %d（delta=0 不改账本）", got, want)
	}
	// ③ 目录级配额走 EnsureScope —— 与 EnsureChild 同一记账（子树逐级）。
	if d := s.EnsureScope([]string{"user", "videos"}, 200); d == nil {
		t.Fatal("目录配额 200（≤ alice 600）不得被拒")
	}
	// carve-out：videos 的额度记在**直接父 user** 上（中间层 user 无上限 ⇒ 其 allocated 反映
	// "切给子节点的额度"）；**不**记到 alice/root（那是父额度的再切分 ⇒ 上溯会双计）。
	userSc := s.Resolve([]string{"user"})
	if got, want := userSc.Allocated(), int64(200); got != want {
		t.Fatalf("user.Allocated()=%d want %d（videos 额度记在直接父）", got, want)
	}
	if got := s.Allocated(); got != 0 {
		t.Fatalf("alice.Allocated()=%d want 0（其直接子 user 无上限）", got)
	}
	if got, want := root.Allocated(), int64(600); got != want {
		t.Fatalf("root.Allocated()=%d want %d（只含直接子 alice）", got, want)
	}
	// ④ I1 强制：Σ 租户配额(600+600) > 全局(1000) ⇒ **建树即拒绝**且**原子**（不改状态）。
	if got := root.Scope("/tenant/bob", 600); got != nil {
		t.Fatal("Σ 租户配额 1200 > 全局 1000 必须被拒绝（返回 nil；fail-closed）")
	}
	if got, want := root.Allocated(), int64(600); got != want {
		t.Fatalf("I1 拒绝必须原子，root.Allocated()=%d want %d", got, want)
	}
	// ⑤ 合法余量内的租户仍可建（400 剩余 ⇒ 200 可以）。
	if got := root.Scope("/tenant/carol", 400); got == nil {
		t.Fatal("Σ=1000 ≤ 1000 应可建")
	}
}
