// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// quota_owner_scope_test.go —— per-owner 配额用量读取（配额预警面）回归。
//
// 背景（设计 `2026-10-16-quota-volume-layer.md` §9.2）：配额预警引擎的用量读取器此前用
// `h.quotaScopeFor(owner, "")`，而 `quotaScopeFor` 首行 `segs := strings.Split(rel, "/")`
// 在 `rel == ""` 时得 `segs[0] == ""` ⇒ **恒返回 nil** ⇒ 读取器**永远得到 (0,0)** ⇒
// **per-owner 配额预警静默失效**（无日志/指标/报错）。
//
// 修复 = 改用 owner **根 Scope**。为何它的 `Usage()` 就是「该 owner 的用量」（2026-10-16 核实）：
// `type Scope struct{ pool *Pool }`，`Mount`/`EnsureScope` 经 `newScope` 创建**子 Pool**
// （`parent: p`，各自持有 `committed`/`maxBytes`）⇒ `Scope.Usage()` 返回 `p.committed`
// = **该节点自己的**用量（**不是**全局池用量）；且 `commitUp` 沿 `parent` 链逐层累计
// ⇒ 根节点 `committed` = **该 owner 全子树用量**。
//
// 变异：把 `quota.Pool.Usage()` 改成返回**根节点**的 `committed`（即我一度误以为的「全局池语义」）
// ⇒ 用例 ④（bob 必须为 0）红 —— 这正是本用例的**判别力**所在。

import "testing"

// TestOwnerQuotaScope_AggregatesSubtreeAndIsolatesOwners 断言四件事：
//  1. `quotaScopeFor(owner, "")` **恒 nil**（把陷阱文档化，防后人再踩）；
//  2. `ownerQuotaScope(owner)` 非 nil；
//  3. 子桶结算 ⇒ **根聚合可见**（`commitUp` 沿父链累计）；
//  4. **判别性**：另一 owner 的用量**不**计入（证明是**节点聚合**，不是全局池 ✗）。
func TestOwnerQuotaScope_AggregatesSubtreeAndIsolatesOwners(t *testing.T) {
	t.Parallel()
	h := newAssemblyTestHandlers(t, t.TempDir())

	// ① 陷阱：空 rel ⇒ nil（预警读取器此前因此静默失效）。
	if sc := h.quotaScopeFor("alice", ""); sc != nil {
		t.Fatal(`quotaScopeFor(owner, "") 应恒为 nil（rel 首段为空 ⇒ 非功能桶）`)
	}
	// ② 正确入口非 nil。
	aliceRoot := h.ownerQuotaScope("alice")
	if aliceRoot == nil {
		t.Fatal("ownerQuotaScope 应返回 owner 根 Scope（globalPool 已装配）")
	}
	if u := aliceRoot.Usage(); u != 0 {
		t.Fatalf("初始用量应为 0，got %d", u)
	}
	// ③ 在 alice 的子桶里结算 4096 ⇒ 根聚合可见。
	sub := h.quotaScopeFor("alice", "user/a.bin")
	if sub == nil {
		t.Fatal("功能桶首段应可解析出子 Scope")
	}
	res, err := sub.TryReserve(4096)
	if err != nil || res == nil {
		t.Fatalf("TryReserve: res=%v err=%v", res, err)
	}
	res.Commit(4096)
	if u := h.ownerQuotaScope("alice").Usage(); u != 4096 {
		t.Fatalf("alice 根用量=%d，want 4096（commitUp 应沿父链累计到根）", u)
	}
	// ④ **判别性**：bob 根用量必须为 0。
	// 若 `Usage()` 实为「全局池用量」（我一度误以为如此），此处会得到 4096 ⇒ 红。
	if u := h.ownerQuotaScope("bob").Usage(); u != 0 {
		t.Fatalf("bob 根用量=%d，want 0（owner 间必须隔离）", u)
	}
	// ⑤ **判别性核心**：**同一 owner 的两个功能桶之间**也必须隔离 ——
	// 这是区分「节点用量 ✓」与「全局池用量 ✗」的**唯一**断言（根 scope 断言无法区分：
	// 根节点无论哪种语义都返回 4096）。若 `Usage()` 实为全局池语义 ⇒ 此处会得到 4096 ⇒ 红。
	if u := h.quotaScopeFor("alice", "cloud/x.bin").Usage(); u != 0 {
		t.Fatalf("alice 的 cloud 桶用量=%d，want 0（节点级隔离；若为全局池用量则为 4096 ⇒ 红）", u)
	}
}
