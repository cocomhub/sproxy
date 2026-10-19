// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package recovery

// recovery_test.go 锁定 M3 协调器语义（设计 §6.3 + docs/archive/architecture-task-recovery.md）：
//  ① **恢复**：未完成作用域 ⇒ Resume（按 Seq 重放）⇒ **成功后**写提交记录（MarkDone）；
//  ② **跳过**：已提交作用域零动作；
//  ③ **未注册前缀不静默丢弃**：计入 Unknown 且**保持未完成**（可补注册后恢复）；
//  ④ **失败保持未完成**：apply 出错 ⇒ Failed 计数 + 作用域仍 unfinished ⇒ 下次重试可成功；
//  ⑤ **幂等**：重复 Run 对已提交作用域零动作；
//  ⑥ 注册校验（空字段/前缀冲突）+ 最长前缀优先。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// recorder 记录 apply 顺序（测试替身）。
type recorder struct {
	seqs []uint64
	fail error
}

func (r *recorder) Apply(_ context.Context, _ audit.ScopeID, seq uint64, state []byte) error {
	if r.fail != nil {
		return r.fail
	}
	var rec audit.StateRecord
	if err := json.Unmarshal(state, &rec); err != nil {
		return err
	}
	r.seqs = append(r.seqs, seq)
	return nil
}

// newAudit 建一个真实落盘的 audit.Service（重启语义由 Journal 保证）。
func newAudit(t *testing.T) *audit.Service {
	t.Helper()
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir:      t.TempDir(),
		Layout:   audit.LayoutPerScope,
		SyncMode: audit.SyncAlways,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	caps := audit.FullCapabilities()
	s, err := audit.New(audit.Options{Journal: j, Caps: &caps}) // Clock 缺省 = 系统时钟
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return s
}

// quietLogger 避免测试输出噪音。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestCoordinator_ResumesUnfinishedScope 未完成作用域被重放并提交。
func TestCoordinator_ResumesUnfinishedScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, err := svc.Open(ctx, "task.cloud.t1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, v := range []int{1, 2} {
		if serr := sc.Save(ctx, "step", v); serr != nil {
			t.Fatalf("Save: %v", serr)
		}
	}
	rec := &recorder{}
	c := New(svc, quietLogger())
	// 注册表在测试内构造（装配层负责生产注册）。
	if rerr := c.Register(Registration{
		Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true },
	}); rerr != nil {
		t.Fatalf("Register: %v", rerr)
	}
	rep, rerr := c.Run(ctx, Options{})
	if rerr != nil {
		t.Fatalf("Run: %v", rerr)
	}
	if rep.Total != 1 || rep.Resumed != 1 || rep.Failed != 0 {
		t.Fatalf("报告不符: %s", rep)
	}
	if len(rec.seqs) != 2 || rec.seqs[0] >= rec.seqs[1] {
		t.Fatalf("应按 Seq 升序重放 2 条, got %v", rec.seqs)
	}
	// 提交记录已写 ⇒ 不再出现在未完成列表。
	left, lerr := svc.ListScopes(ctx, audit.ScopeStateFilter{UnfinishedOnly: true})
	if lerr != nil {
		t.Fatalf("ListScopes: %v", lerr)
	}
	if len(left) != 0 {
		t.Fatalf("恢复后不应残留未完成作用域: %+v", left)
	}
}

// TestCoordinator_SkipsDoneScope 已提交作用域零动作。
func TestCoordinator_SkipsDoneScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.cloud.t2")
	if serr := sc.Save(ctx, "k", "v"); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	if merr := sc.MarkDone(ctx); merr != nil {
		t.Fatalf("MarkDone: %v", merr)
	}
	rec := &recorder{}
	c := New(svc, quietLogger())
	_ = c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true }})
	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Total != 0 || rep.Resumed != 0 {
		t.Fatalf("已完成作用域不应被处理: %s", rep)
	}
	if len(rec.seqs) != 0 {
		t.Fatalf("已提交作用域不得重放, got %v", rec.seqs)
	}
}

// TestCoordinator_UnknownPrefixReported 未注册前缀：可观测且保持未完成。
func TestCoordinator_UnknownPrefixReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.mystery.x1")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	c := New(svc, quietLogger())
	_ = c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return &recorder{}, true }})
	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Unknown != 1 || rep.Skipped != 1 {
		t.Fatalf("未注册前缀应计入 Unknown: %s", rep)
	}
	left, _ := svc.ListScopes(ctx, audit.ScopeStateFilter{UnfinishedOnly: true})
	if len(left) != 1 {
		t.Fatalf("未注册作用域必须**保持未完成**（补注册后可恢复）, got %+v", left)
	}
}

// TestCoordinator_ApplierErrorKeepsScopeUnfinished 失败保持未完成 ⇒ 下次重试成功。
func TestCoordinator_ApplierErrorKeepsScopeUnfinished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.cloud.t3")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	bad := &recorder{fail: errors.New("下游不可用")}
	c := New(svc, quietLogger())
	_ = c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return bad, true }})
	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run（默认不中断）: %v", err)
	}
	if rep.Failed != 1 || rep.Err() == nil {
		t.Fatalf("失败应计入 Failed 且报告带错误: %s", rep)
	}
	left, _ := svc.ListScopes(ctx, audit.ScopeStateFilter{UnfinishedOnly: true})
	if len(left) != 1 {
		t.Fatalf("失败的作用域必须保持未完成（可重试）, got %+v", left)
	}
	// 重试：换成功用的 applier ⇒ 恢复完成。
	good := &recorder{}
	c2 := New(svc, quietLogger())
	_ = c2.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return good, true }})
	rep2, err2 := c2.Run(ctx, Options{})
	if err2 != nil || rep2.Resumed != 1 {
		t.Fatalf("重试应成功: rep=%s err=%v", rep2, err2)
	}
	if len(good.seqs) != 1 {
		t.Fatalf("重试应重放 1 条, got %v", good.seqs)
	}
}

// TestCoordinator_RunIsIdempotent 重复 Run 对已提交作用域零动作。
func TestCoordinator_RunIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t)
	sc, _ := svc.Open(ctx, "task.cloud.t4")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	rec := &recorder{}
	c := New(svc, quietLogger())
	_ = c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true }})
	if _, err := c.Run(ctx, Options{}); err != nil {
		t.Fatalf("Run#1: %v", err)
	}
	rep2, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run#2: %v", err)
	}
	if rep2.Total != 0 || rep2.Resumed != 0 {
		t.Fatalf("第二次运行不应处理任何作用域: %s", rep2)
	}
	if len(rec.seqs) != 1 {
		t.Fatalf("副作用不得重复执行, got %v", rec.seqs)
	}
}

// TestCoordinator_RegisterValidation 注册校验 + 最长前缀优先。
func TestCoordinator_RegisterValidation(t *testing.T) {
	t.Parallel()
	c := New(newAudit(t), quietLogger())
	ok := func(audit.ScopeID) (audit.ReplayApplier, bool) { return &recorder{}, true }
	cases := []Registration{
		{Kind: "", Prefix: "task.x", Factory: ok},
		{Kind: "k", Prefix: "", Factory: ok},
		{Kind: "k", Prefix: "task.x", Factory: nil},
	}
	for i, reg := range cases {
		if err := c.Register(reg); err == nil {
			t.Fatalf("用例 %d 应被拒绝: %+v", i, reg)
		}
	}
	if err := c.Register(Registration{Kind: "root", Prefix: "task", Factory: ok}); err != nil {
		t.Fatalf("Register(task): %v", err)
	}
	if err := c.Register(Registration{Kind: "cloud", Prefix: "task.cloud", Factory: ok}); err != nil {
		t.Fatalf("Register(task.cloud): %v", err)
	}
	if err := c.Register(Registration{Kind: "dup", Prefix: "task.cloud.", Factory: ok}); !errors.Is(err, ErrDuplicatePrefix) {
		t.Fatalf("重复前缀应返回 ErrDuplicatePrefix, got %v", err)
	}
	regs := c.Registrations()
	if len(regs) != 2 || regs[0].Prefix != "task.cloud" {
		t.Fatalf("注册快照应按前缀长度降序: %+v", regs)
	}
	if reg, found := c.match("task.cloud.t1"); !found || reg.Kind != "cloud" {
		t.Fatalf("最长前缀应优先匹配 cloud, got %+v found=%v", reg, found)
	}
	if reg, found := c.match("task.other:t1"); !found || reg.Kind != "root" {
		t.Fatalf("应回落到 task 前缀, got %+v found=%v", reg, found)
	}
	if _, found := c.match("other.scope"); found {
		t.Fatal("无匹配前缀应返回 found=false")
	}
}

// TestCoordinator_NilServiceFailsClosed 未配置 audit.Service ⇒ 明确报错（恢复不可静默失效）。
func TestCoordinator_NilServiceFailsClosed(t *testing.T) {
	t.Parallel()
	c := New(nil, quietLogger())
	if _, err := c.Run(context.Background(), Options{}); err == nil {
		t.Fatal("未配置 Service 时必须返回错误（fail-closed）")
	}
}
