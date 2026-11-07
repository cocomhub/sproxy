// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package recovery

// no_scope_scan_test.go 锁定评审 R13 P1：**无作用域注册时不得扫描审计段**。
//
// 生产装配只注册 legacy（云任务），作用域注册为空。此前 `listUnfinished` 只判 `svc == nil`，
// 于是每次启动都会把整个合规日志（默认不轮转，可达 GiB）读进内存排序，并把 `system` 作用域
// （审计事件恒写该作用域、永不 MarkDone）永久判为「未注册前缀 + 保持未完成」，产生两条恒假告警。

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestRun_NoScopeRegistrationsSkipsScan 只有 legacy ⇒ 不扫作用域（Total/Unknown 为 0），legacy 照常执行。
func TestRun_NoScopeRegistrationsSkipsScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newAudit(t) // 复用既有 helper（真实 Service + 临时 journal）

	// 造一个「看起来未完成」的作用域（只有状态、没有提交记录）。
	if err := svc.Emit(ctx, audit.Entry{
		Scope: "task.abc", Kind: audit.KindState, Action: "step",
		Type: "sensitive", Result: "pending", TS: time.Now(),
	}); err != nil {
		t.Fatalf("Emit state: %v", err)
	}

	legacyRan := false
	c := New(svc, nil)
	if err := c.RegisterLegacy(Legacy{Kind: "cloud", Recover: func(context.Context) (int, error) {
		legacyRan = true
		return 2, nil
	}}); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}

	rep, err := c.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Total != 0 || rep.Unknown != 0 {
		t.Fatalf("无作用域注册时不应扫描: total=%d unknown=%d（会全量读审计段）", rep.Total, rep.Unknown)
	}
	if !legacyRan || rep.LegacyRecovered != 2 {
		t.Fatalf("legacy 应照常执行: ran=%v recovered=%d", legacyRan, rep.LegacyRecovered)
	}
}
