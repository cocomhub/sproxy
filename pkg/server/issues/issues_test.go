// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issues

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock 让重试时间判定可测（不依赖真实时间流逝，测试可并行）。
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func ctx1(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// TestReport_NewItemIsOpenAndRetryable 锁定：新登记事项默认 open；可自动重试的带下次重试时间。
func TestReport_NewItemIsOpenAndRetryable(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore()})

	it := m.Report(Item{
		Kind:      KindDirMetaCleanupFailed,
		Volume:    "baidu",
		Path:      "OypEIkI8i9Y5/abc",
		Detail:    "删除旧目录 meta 失败：网络 5xx",
		Retryable: true,
	})
	if it.Status != StatusOpen {
		t.Fatalf("新事项状态 = %q，期望 open", it.Status)
	}
	if it.ID == "" {
		t.Fatal("应生成稳定 ID")
	}
	if it.Attempts != 0 {
		t.Fatalf("Attempts = %d，期望 0", it.Attempts)
	}
	if it.NextRetryAt.IsZero() {
		t.Fatal("可重试事项应设置 NextRetryAt")
	}
	if it.FirstSeen.IsZero() || it.LastSeen.IsZero() {
		t.Fatal("应记录首次/最近发现时间")
	}
}

// TestReport_NeedsHumanIsNotRetried 锁定用户核心要求：
// **无法自主决策的残留数据必须人工感知** ⇒ 标 needs_human 且**不自动重试**。
func TestReport_NeedsHumanIsNotRetried(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore()})

	it := m.Report(Item{
		Kind:      KindDirMetaAmbiguous,
		Volume:    "baidu",
		Path:      "c1",
		Detail:    "容器内有 2 个 dir-meta（异常布局）；需人工仲裁后清理",
		Retryable: false, // 不可自主决策
	})
	if it.Status != StatusNeedsHuman {
		t.Fatalf("不可自主决策的事项状态 = %q，期望 needs_human", it.Status)
	}
	if got := m.Due(ctx1(t), clk.now.Add(24*time.Hour)); len(got) != 0 {
		t.Fatalf("needs_human 不得进入自动重试队列，got %d", len(got))
	}
}

// TestReport_IdempotentByKindVolumePath 锁定：同一 (Kind,Volume,Path) 重复登记**合并**为一条
// （累加次数、更新 LastSeen/Detail），不产生重复条目 —— 否则重试循环会无限增长。
func TestReport_IdempotentByKindVolumePath(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore()})
	base := Item{Kind: KindDirMetaCleanupFailed, Volume: "baidu", Path: "c1", Detail: "第一次失败", Retryable: true}

	a := m.Report(base)
	clk.Advance(time.Minute)
	base.Detail = "第二次失败（新原因）"
	b := m.Report(base)

	if a.ID != b.ID {
		t.Fatalf("同一 (Kind,Volume,Path) 应合并为同一条：%s vs %s", a.ID, b.ID)
	}
	if b.Occurrences != 2 {
		t.Fatalf("Occurrences = %d，期望 2", b.Occurrences)
	}
	if b.Detail != "第二次失败（新原因）" {
		t.Fatalf("Detail 应更新为最新原因，got %q", b.Detail)
	}
	if !b.LastSeen.After(b.FirstSeen) {
		t.Fatal("LastSeen 应晚于 FirstSeen")
	}
	if got := len(m.List(ListFilter{})); got != 1 {
		t.Fatalf("应只有 1 条，got %d", got)
	}
}

// TestDue_TakesExpiredOpenItems 锁定：只把**到期且 open/retrying** 的事项交给重试器。
func TestDue_TakesExpiredOpenItems(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore(), RetryBackoff: time.Minute})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "v", Path: "p1", Retryable: true})

	if got := m.Due(ctx1(t), clk.now); len(got) != 0 {
		t.Fatalf("刚登记（未到期）不应进入重试，got %d", len(got))
	}
	clk.Advance(2 * time.Minute)
	if got := m.Due(ctx1(t), clk.now); len(got) != 1 {
		t.Fatalf("到期后应进入重试，got %d", len(got))
	}
}

// TestResolve_AutoRetrySucceeds 锁定「最终必须成功」：可重试事项在执行器成功后置 resolved。
func TestResolve_AutoRetrySucceeds(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore(), RetryBackoff: time.Minute})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "v", Path: "p1", Retryable: true})
	clk.Advance(2 * time.Minute)

	calls := 0
	res := m.RunDue(ctx1(t), clk.now, map[Kind]RetryFunc{
		KindDirMetaCleanupFailed: func(context.Context, Item) error { calls++; return nil },
	})
	if calls != 1 {
		t.Fatalf("执行器应被调用 1 次，got %d", calls)
	}
	if res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	id := m.List(ListFilter{})[0].ID
	it, ok := m.Get(id)
	if !ok || it.Status != StatusResolved {
		t.Fatalf("应置为 resolved：%+v", it)
	}
	if it.ResolvedAt.IsZero() {
		t.Fatal("应记录解决时间")
	}
	if len(m.Due(ctx1(t), clk.now.Add(time.Hour))) != 0 {
		t.Fatal("已解决事项不应再进重试队列")
	}
}

// TestRunDue_BoundedRetriesThenNeedsHuman 锁定「有界 + 最终转人工」：
// 持续失败 ⇒ 重试到上限后**转 needs_human**（而不是无限重试或静默放弃）。
func TestRunDue_BoundedRetriesThenNeedsHuman(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore(), RetryBackoff: time.Minute, MaxAttempts: 3})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "v", Path: "p1", Retryable: true})

	fail := map[Kind]RetryFunc{
		KindDirMetaCleanupFailed: func(context.Context, Item) error { return errors.New("远端 5xx") },
	}
	var last Item
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Minute)
		m.RunDue(ctx1(t), clk.now, fail)
		last, _ = m.Get(m.List(ListFilter{})[0].ID)
		if last.Status == StatusNeedsHuman {
			break
		}
	}
	if last.Status != StatusNeedsHuman {
		t.Fatalf("达到重试上限后应转 needs_human（当前 attempts=%d status=%q）", last.Attempts, last.Status)
	}
	if last.LastError == "" {
		t.Fatal("应保留最后一次失败原因（供人工判断）")
	}
	if last.Attempts != 3 {
		t.Fatalf("Attempts 应停在上限 3，got %d", last.Attempts)
	}
	if len(m.Due(ctx1(t), clk.now.Add(100*time.Hour))) != 0 {
		t.Fatal("needs_human 后不得继续自动重试")
	}
}

// TestRunDue_NoExecutorMarksNeedsHuman 锁定：没有为该 Kind 注册执行器时**不得**静默消费，
// 应转 needs_human（人工接入）—— 避免"以为是自动重试其实永远没跑"。
func TestRunDue_NoExecutorMarksNeedsHuman(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore(), RetryBackoff: time.Minute})
	m.Report(Item{Kind: KindContainerNotIndexed, Volume: "v", Path: "p1", Retryable: true})
	clk.Advance(2 * time.Minute)

	m.RunDue(ctx1(t), clk.now, nil)
	it, _ := m.Get(m.List(ListFilter{})[0].ID)
	if it.Status != StatusNeedsHuman {
		t.Fatalf("无执行器应转 needs_human，got %q", it.Status)
	}
	if it.LastError == "" {
		t.Fatal("应写明原因（无自动处理能力）")
	}
}

// TestList_Filters 锁定 UI/API 需要的筛选能力（按状态、按卷、按类型、仅未解决）。
func TestList_Filters(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore()})
	m.Report(Item{Kind: KindDirMetaAmbiguous, Volume: "baidu", Path: "p1"})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "baidu", Path: "p2", Retryable: true})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "main", Path: "p3", Retryable: true})

	if got := m.List(ListFilter{Status: StatusNeedsHuman}); len(got) != 1 {
		t.Fatalf("按状态筛选 = %d，期望 1", len(got))
	}
	if got := m.List(ListFilter{Volume: "baidu"}); len(got) != 2 {
		t.Fatalf("按卷筛选 = %d，期望 2", len(got))
	}
	if got := m.List(ListFilter{Kind: KindDirMetaCleanupFailed}); len(got) != 2 {
		t.Fatalf("按类型筛选 = %d，期望 2", len(got))
	}
	if got := m.List(ListFilter{UnresolvedOnly: true}); len(got) != 3 {
		t.Fatalf("未解决列表 = %d，期望 3", len(got))
	}
}

// TestPersistence_SurvivesReload 锁定：事项与重试进度**必须持久化**（重启后仍可见、可继续重试）。
func TestPersistence_SurvivesReload(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	st := NewMemStore()
	m := New(Options{Clock: clk.Now, Store: st, RetryBackoff: time.Minute})
	m.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "v", Path: "p1", Detail: "d1", Retryable: true})

	m2 := New(Options{Clock: clk.Now, Store: st})
	got := m2.List(ListFilter{})
	if len(got) != 1 || got[0].Detail != "d1" {
		t.Fatalf("重载后应保留事项：%+v", got)
	}
}

// TestResolveManual_NeedsHumanBecomesResolved 锁定人工接入闭环：
// 人工处理后调用 `Resolve`（或 `/api/issues/{id}/resolve`）⇒ 事项转 resolved 并从列表消失。
func TestResolveManual_NeedsHumanBecomesResolved(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	m := New(Options{Clock: clk.Now, Store: NewMemStore()})
	m.Report(Item{Kind: KindDirMetaAmbiguous, Volume: "baidu", Path: "c1", Detail: "需人工仲裁"})

	id := m.List(ListFilter{})[0].ID
	if !m.Resolve(id, "人工已仲裁并清理旧 meta（保留 Version 较大者）") {
		t.Fatal("Resolve 应返回 true")
	}
	it, _ := m.Get(id)
	if it.Status != StatusResolved {
		t.Fatalf("状态 = %q，期望 resolved", it.Status)
	}
	if it.Resolution == "" {
		t.Fatal("应记录人工处理说明（审计）")
	}
	if got := m.List(ListFilter{UnresolvedOnly: true}); len(got) != 0 {
		t.Fatalf("解决后未解决列表应为空，got %d", len(got))
	}
}
