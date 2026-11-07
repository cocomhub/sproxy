// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package recovery

// legacy_test.go 锁定 M3「legacy 兜底」语义（设计 M3：首版保留既有子系统恢复，收敛**触发点**）：
//  ① legacy 入口被执行且计数；
//  ② legacy 失败**隔离**：不影响其它 legacy 入口，也不阻断作用域重放；
//  ③ **Kind 互斥**：同一任务族不能既是 legacy 又是作用域恢复（否则一次 Run 恢复两遍）；
//  ④ 顺序：legacy →（资源就绪）→ 作用域重放；
//  ⑤ panic 兜住（E21 同类：恢复路径不得把启动打崩）。

import (
	"context"
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestCoordinator_LegacyRecovererRuns legacy 入口被执行并计入报告。
func TestCoordinator_LegacyRecovererRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := New(newAudit(t), quietLogger())
	var calls int
	if err := c.RegisterLegacy(Legacy{Kind: "chunked", Recover: func(context.Context) (int, error) {
		calls++
		return 3, nil
	}}); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}
	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("legacy 入口应执行 1 次, got %d", calls)
	}
	if rep.LegacyRecovered != 3 || len(rep.Legacy) != 1 || rep.Legacy[0].Kind != "chunked" {
		t.Fatalf("报告不符: %s legacy=%+v", rep, rep.Legacy)
	}
}

// TestCoordinator_LegacyErrorIsolated 失败隔离：一个 legacy 坏掉不拖住其它与作用域重放。
func TestCoordinator_LegacyErrorIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.cloud.t1")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	rec := &recorder{}
	c := New(svc, quietLogger())
	_ = c.RegisterLegacy(Legacy{Kind: "bad", Recover: func(context.Context) (int, error) {
		return 0, errors.New("下游不可用")
	}})
	_ = c.RegisterLegacy(Legacy{Kind: "good", Recover: func(context.Context) (int, error) { return 1, nil }})
	_ = c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true }})
	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("默认不应中断: %v", err)
	}
	if rep.Resumed != 1 {
		t.Fatalf("legacy 失败不得阻断作用域重放: %s", rep)
	}
	if rep.LegacyRecovered != 1 || len(rep.Legacy) != 2 {
		t.Fatalf("其它 legacy 入口应继续执行: %+v", rep.Legacy)
	}
	if rep.Failed != 1 || rep.Err() == nil {
		t.Fatalf("失败应计入 Failed 且报告带错误: %s", rep)
	}
	// FailFast：legacy 失败即中止（作用域重放不再进行）。
	svc2 := newAudit(t)
	sc2, _ := svc2.Open(ctx, "task.cloud.t2")
	if serr := sc2.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	rec2 := &recorder{}
	c2 := New(svc2, quietLogger())
	_ = c2.RegisterLegacy(Legacy{Kind: "bad", Recover: func(context.Context) (int, error) {
		return 0, errors.New("下游不可用")
	}})
	_ = c2.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec2, true }})
	if _, ferr := c2.Run(ctx, Options{FailFast: true}); ferr == nil {
		t.Fatal("FailFast 时 legacy 失败应返回错误")
	}
	if len(rec2.seqs) != 0 {
		t.Fatalf("FailFast 中止后不得继续重放, got %v", rec2.seqs)
	}
}

// TestCoordinator_LegacyKindExclusive 同一 Kind 只能注册一种恢复形态（双向）。
func TestCoordinator_LegacyKindExclusive(t *testing.T) {
	t.Parallel()
	c := New(newAudit(t), quietLogger())
	ok := func(audit.ScopeID) (audit.ReplayApplier, bool) { return &recorder{}, true }
	recoverOK := func(context.Context) (int, error) { return 0, nil }
	if err := c.Register(Registration{Kind: "cloud", Prefix: "task.cloud", Factory: ok}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.RegisterLegacy(Legacy{Kind: "cloud", Recover: recoverOK}); !errors.Is(err, ErrDuplicatePrefix) {
		t.Fatalf("同 Kind 已注册作用域恢复时应拒绝 legacy, got %v", err)
	}
	c2 := New(newAudit(t), quietLogger())
	if err := c2.RegisterLegacy(Legacy{Kind: "sync", Recover: recoverOK}); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}
	if err := c2.Register(Registration{Kind: "sync", Prefix: "task.sync", Factory: ok}); !errors.Is(err, ErrDuplicatePrefix) {
		t.Fatalf("同 Kind 已注册 legacy 时应拒绝作用域恢复, got %v", err)
	}
	if err := c2.RegisterLegacy(Legacy{Kind: "sync", Recover: recoverOK}); err == nil {
		t.Fatal("legacy Kind 重复注册应被拒绝")
	}
	if err := c2.RegisterLegacy(Legacy{Kind: "", Recover: recoverOK}); err == nil {
		t.Fatal("空 Kind 应被拒绝")
	}
	if err := c2.RegisterLegacy(Legacy{Kind: "x", Recover: nil}); err == nil {
		t.Fatal("nil Recover 应被拒绝")
	}
}

// TestCoordinator_LegacyRunsBeforeScopeReplay 顺序：legacy 先（资源就绪）→ 作用域重放。
func TestCoordinator_LegacyRunsBeforeScopeReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.cloud.t1")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	var order []string
	c := New(svc, quietLogger())
	if err := c.RegisterLegacy(Legacy{Kind: "storage", Recover: func(context.Context) (int, error) {
		order = append(order, "legacy")
		return 0, nil
	}}); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}
	if err := c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) {
			order = append(order, "scope")
			return &recorder{}, true
		}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := c.Run(ctx, Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(order) != 2 || order[0] != "legacy" || order[1] != "scope" {
		t.Fatalf("恢复顺序应为 legacy → scope, got %v", order)
	}
}

// TestCoordinator_LegacyPanicRecovered panic 被兜住（恢复路径不得把启动打崩，E21 同类）。
func TestCoordinator_LegacyPanicRecovered(t *testing.T) {
	t.Parallel()
	c := New(newAudit(t), quietLogger())
	_ = c.RegisterLegacy(Legacy{Kind: "boom", Recover: func(context.Context) (int, error) {
		panic("子系统内部 panic")
	}})
	rep, err := c.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run 不应因 panic 中断: %v", err)
	}
	if len(rep.Legacy) != 1 || rep.Legacy[0].Err == nil {
		t.Fatalf("panic 应转为错误计入报告: %+v", rep.Legacy)
	}
	if rep.Failed != 1 {
		t.Fatalf("panic 应计入 Failed: %s", rep)
	}
}
