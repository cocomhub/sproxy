// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// ensurechild_crosslevel_test.go 钉住配额模型 v3 的**分配不变式**（独立审核 P2，2026-10-18）：
// 文件头自述「Σ 子节点上限 ≤ 父节点上限」必须**在每一层**成立，且分配账本更新必须**原子**
// （并发 EnsureChild 不得把父 allocated 记重 ⇒ 后续合法分配假失败）。

import (
	"testing"
)

// TestEnsureChild_RejectsChildExceedingIntermediateLimit 中间层上限必须约束其子节点。
func TestEnsureChild_RejectsChildExceedingIntermediateLimit(t *testing.T) {
	t.Parallel()
	p := NewPool(1000)
	a, err := p.EnsureChild([]string{"a"}, 100)
	if err != nil {
		t.Fatalf("前置 EnsureChild(a,100): %v", err)
	}
	// b.max(200) > a.max(100) ⇒ 必须拒绝（未修必红：旧实现只校验起始节点 p ⇒ 放行 ⇒
	// 破坏「Σ 子上限 ≤ 父上限」不变式）。
	if _, err := p.EnsureChild([]string{"a", "b"}, 200); err == nil {
		t.Fatal("b.max(200) > a.max(100) 必须被拒绝（未修必红：只校验起始节点 ⇒ 不变式被破坏）")
	}
	// 全回滚：失败不得改动任何状态（既有承诺）。
	// 注：a.Allocated() 是「a 的**子节点**上限之和」（a 此时无子 ⇒ 0）；b 的上限记在 a 上。
	if got := a.Allocated(); got != 0 {
		t.Fatalf("失败后 a.allocated 必须仍为 0（b 未建立 ⇒ 全回滚），实际 %d", got)
	}
	if got := p.Allocated(); got != 100 {
		t.Fatalf("失败后 root.allocated 必须仍为 100（全回滚），实际 %d", got)
	}
	// 对照：合法额度（≤ 中间层上限）必须成功，且记在中间层 a 上。
	if _, err := p.EnsureChild([]string{"a", "b"}, 100); err != nil {
		t.Fatalf("b.max(100) ≤ a.max(100) 应成功: %v", err)
	}
	if got := a.Allocated(); got != 100 {
		t.Fatalf("成功后 a.allocated=100（记在其直接子 b 上），实际 %d", got)
	}
	// **carve-out 语义 + 形态无关**判别断言：allocated = Σ **直接子**上限 ⇒ b 的上限记在 a 上，
	// root 只含 a ⇒ root.allocated == 100。关键不是这个数，而是**形态无关**：同样的逻辑树用
	// `a.EnsureChild(["b"],100)` 装配必须得到同一结果（见 TestEnsureChild_FormIndependent）。
	if got := p.Allocated(); got != 100 {
		t.Fatalf("成功后 root.allocated=100（只含直接子 a），实际 %d", got)
	}
}

// TestEnsureChild_FormIndependent 钉住**结构不变式：同一逻辑树与调用形态无关**（独立复审 P1）。
//
// 缺陷（我的首版修法）：逐级记账只上溯到**调用目标 p** ⇒ 同一棵树用 Pool 形态装配会被拒、
// 用 Scope 形态却通过（实测：`p.EnsureChild(["a"],1000)` 后 `p.EnsureChild(["a","b"],1)` 报
// 「分配超限」，而 `a.EnsureChild(["b"],1)` 通过）。修法：上溯到**全局根**。
func TestEnsureChild_FormIndependent(t *testing.T) {
	t.Parallel()
	const rootMax, aMax, bMax = 1000, 900, 50
	// 形态一：从根一次给全路径。
	p1 := NewPool(rootMax)
	if _, err := p1.EnsureChild([]string{"a"}, aMax); err != nil {
		t.Fatalf("形态一 步骤1: %v", err)
	}
	_, err1 := p1.EnsureChild([]string{"a", "b"}, bMax)
	// 形态二：先建 a，再从 a 建 b（同一逻辑树）。
	p2 := NewPool(rootMax)
	a2, err := p2.EnsureChild([]string{"a"}, aMax)
	if err != nil {
		t.Fatalf("形态二 步骤1: %v", err)
	}
	_, err2 := a2.EnsureChild([]string{"b"}, bMax)

	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("同一逻辑树因调用形态不同结果不同（形态相关 ⇒ 缺陷）：Pool=%v Scope=%v", err1, err2)
	}
	if err1 != nil {
		t.Fatalf("合法树（Σ子树上限 %d+%d ≤ %d）不应被拒: %v", aMax, bMax, rootMax, err1)
	}
	if got, want := p1.Allocated(), p2.Allocated(); got != want {
		t.Fatalf("形态一 root.allocated=%d ≠ 形态二 %d（形态相关）", got, want)
	}
	if got, want := p1.Allocated(), int64(aMax); got != want {
		t.Fatalf("root.allocated=%d, want %d（只含直接子 a 的上限；b 记在 a 上）", got, want)
	}
}

// 覆盖边界（如实记录）：**原子性**（并发改额不得把父 allocated 记重）无法构造确定性判别用例——
// 变异验证显示：恢复旧实现（两个临界区）后，下面这类并发用例仍**全绿**（交错窗口过窄，
// 属概率性触发）⇒ 按"不保留无判别力用例"纪律删除，不制造假信心。该性质由**结构**保证：
// 整段读旧值/记账/写新上限都在全局根 allocMu 内（见 allocation.go），并已由上面这条
// **确定性**不变式用例（中间层超额必须被拒绝 + 全回滚）覆盖其可观测后果。
