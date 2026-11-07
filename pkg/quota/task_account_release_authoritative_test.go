// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// task_account_release_authoritative_test.go 钉住「丢弃产物时的回拨金额必须**取自账户权威记录**」
// （独立审核 P2，2026-10-18）。
//
// 缺陷（根因：释放金额来自**非权威外部来源**）：转存失败进入重下流程时，回拨用的是**闭包捕获的
// 旧下载结果 result.Size**（外层 result 只在 transferDone 返回后才更新）⇒ 动态源两次尺寸不同时
// committed 与实际偏离（少释放 ⇒ 幽灵占用 ⇒ 后续同租户下载假 507）。
// 修法：新增 ReleaseAllCommitted —— 金额取自账户自身，调用方**无法**传错尺寸。

import "testing"

func TestTaskAccount_ReleaseAllCommitted_IsAuthoritative(t *testing.T) {
	t.Parallel()
	pool := NewPool(10_000)
	sc := pool.newScope("owner", 10_000)
	// 用 reconcile 路径（0 预留），聚焦「committed 回拨」本身。
	acc := NewTaskAccountReconcile(sc)

	// 模拟：第 1 次下载 300 → 转存失败 → 动态源第 2 次 500 ⇒ 账户记录的是**真实 committed=500**。
	acc.AdjustCommitted(500)
	if got := acc.Committed(); got != 500 {
		t.Fatalf("前置 committed=%d, want 500", got)
	}
	// 权威释放：金额取自账户（调用方无从传错尺寸）。
	// 未修必红：旧路径由调用方传 result.Size（=第 1 次的 300）⇒ 只释放 300 ⇒ 残留 200 幽灵占用。
	if got := acc.ReleaseAllCommitted(); got != 500 {
		t.Fatalf("必须释放账户全部 committed(500)，实际 %d（未修必红：按外部陈旧尺寸释放 ⇒ 少释放 ⇒ 幽灵占用）", got)
	}
	if got := acc.Committed(); got != 0 {
		t.Fatalf("释放后 committed 必须归零，实际 %d", got)
	}
	if got := sc.Pool().committed; got != 0 { // 包内直读（无并发）
		t.Fatalf("Scope 用量必须已回拨（磁盘已删 ⇒ 账本归零），实际 %d", got)
	}
	// 幂等：重复调用不产生负漂移。
	if got := acc.ReleaseAllCommitted(); got != 0 {
		t.Fatalf("幂等：再次调用应返回 0，实际 %d", got)
	}
}
