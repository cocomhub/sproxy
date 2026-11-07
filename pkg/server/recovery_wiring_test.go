// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// recovery_wiring_test.go 锁定 M3 服务端单一起点语义：
//  ① 未注册任务族 ⇒ 跳过（返回 nil）—— 避免每次启动全量扫段（合规段可达 GiB 级）却无任务可恢复；
//  ② 审计不可用 ⇒ 跳过并告警（不静默假装已恢复）；
//  ③ **端到端**：装配 → 启动恢复 ⇒ 未完成作用域被重放并写提交记录（重启续跑链路成立）；
//  ④ 注册表错误（前缀冲突）不阻断启动，但该任务族被跳过。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/recovery"
)

// wiringAudit 建一个落盘的审计服务（测试用）。
func wiringAudit(t *testing.T) *audit.Service {
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
	svc, err := audit.New(audit.Options{Journal: j, Caps: &caps})
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return svc
}

// wiringLogger 静默 logger（避免测试输出噪音）。
func wiringLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wiringApplier 记录重放（测试替身）。
type wiringApplier struct{ seqs []uint64 }

func (a *wiringApplier) Apply(_ context.Context, _ audit.ScopeID, seq uint64, state []byte) error {
	var rec audit.StateRecord
	if err := json.Unmarshal(state, &rec); err != nil {
		return err
	}
	a.seqs = append(a.seqs, seq)
	return nil
}

// TestSetupRecovery_SkipsWithoutRegistrations 无注册任务族 ⇒ 跳过（零行为变更）。
func TestSetupRecovery_SkipsWithoutRegistrations(t *testing.T) {
	t.Parallel()
	svc := wiringAudit(t)
	if c := setupRecovery(svc, wiringLogger(), nil, nil); c != nil {
		t.Fatal("未注册任务族时应返回 nil（跳过，避免空转全量扫段）")
	}
	// runStartupRecovery 对 nil 协调器必须是安全 no-op。
	runStartupRecovery(context.Background(), nil, wiringLogger())
}

// TestSetupRecovery_NilAuditService 审计不可用 ⇒ 跳过。
func TestSetupRecovery_NilAuditService(t *testing.T) {
	t.Parallel()
	reg := recovery.Registration{Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return &wiringApplier{}, true }}
	if c := setupRecovery(nil, wiringLogger(), []recovery.Registration{reg}, nil); c != nil {
		t.Fatal("审计服务为 nil 时应跳过（不静默假装已恢复）")
	}
}

// TestSetupRecovery_ResumesScopeEndToEnd 装配 → 启动恢复 ⇒ 未完成作用域被重放并提交。
func TestSetupRecovery_ResumesScopeEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := wiringAudit(t)
	sc, err := svc.Open(ctx, "task.cloud.t1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if serr := sc.Save(ctx, "step", "download"); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	ap := &wiringApplier{}
	coord := setupRecovery(svc, wiringLogger(), []recovery.Registration{{
		Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return ap, true },
	}}, nil)
	if coord == nil {
		t.Fatal("有注册任务族时应构造协调器")
	}
	runStartupRecovery(ctx, coord, wiringLogger())
	if len(ap.seqs) != 1 {
		t.Fatalf("应重放 1 条状态, got %v", ap.seqs)
	}
	left, lerr := svc.ListScopes(ctx, audit.ScopeStateFilter{UnfinishedOnly: true})
	if lerr != nil {
		t.Fatalf("ListScopes: %v", lerr)
	}
	if len(left) != 0 {
		t.Fatalf("恢复后应无未完成作用域（提交记录已写）, got %+v", left)
	}
	// 再次启动恢复：幂等（零动作、副作用不重放）。
	runStartupRecovery(ctx, setupRecovery(svc, wiringLogger(), []recovery.Registration{{
		Kind: "cloud", Prefix: "task.cloud",
		Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return ap, true },
	}}, nil), wiringLogger())
	if len(ap.seqs) != 1 {
		t.Fatalf("第二次启动不得重放副作用, got %v", ap.seqs)
	}
}

// TestSetupRecovery_BadRegistrationSkipped 注册表错误（前缀冲突）不阻断启动，冲突项被跳过。
func TestSetupRecovery_BadRegistrationSkipped(t *testing.T) {
	t.Parallel()
	svc := wiringAudit(t)
	fac := func(audit.ScopeID) (audit.ReplayApplier, bool) { return &wiringApplier{}, true }
	coord := setupRecovery(svc, wiringLogger(), []recovery.Registration{
		{Kind: "cloud", Prefix: "task.cloud", Factory: fac},
		{Kind: "dup", Prefix: "task.cloud", Factory: fac},
	}, nil)
	if coord == nil {
		t.Fatal("存在有效注册时应构造协调器（冲突项只跳过）")
	}
	if n := len(coord.Registrations()); n != 1 {
		t.Fatalf("冲突前缀应被跳过，仅保留首个注册, got %d", n)
	}
}
