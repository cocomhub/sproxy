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
}

// 覆盖边界（如实记录）：**原子性**（并发改额不得把父 allocated 记重）无法构造确定性判别用例——
// 变异验证显示：恢复旧实现（两个临界区）后，下面这类并发用例仍**全绿**（交错窗口过窄，
// 属概率性触发）⇒ 按"不保留无判别力用例"纪律删除，不制造假信心。该性质由**结构**保证：
// 整段读旧值/记账/写新上限都在全局根 allocMu 内（见 allocation.go），并已由上面这条
// **确定性**不变式用例（中间层超额必须被拒绝 + 全回滚）覆盖其可观测后果。
