// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// upload_recovery_wiring_test.go 锁定 G1：**上传内核的崩溃遗留会话接入 M3 统一恢复**。
//
// 语义要点（严格遵循 uploadcore/recovery.go 的设计决策，测试即契约）：
//  ① sink **幂等**（可重复扫描，不产生重复登记）且返回拷贝（不暴露内部 map）；
//  ② 无审计 ⇒ **不注册**（否则每次启动都记一条假失败噪声）；
//  ③ **端到端**：WAL 里的未完成上传会话经 `recovery.Coordinator` 扫描后被登记，且
//     **不 MarkDone**（作用域保持未完成 ⇒ 真正上传同一文件时仍能续传）；
//  ④ 重复扫描不改变登记数（幂等）。

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// TestUploadResumeSink_PutResumeIdempotent sink 幂等 + 快照隔离。
func TestUploadResumeSink_PutResumeIdempotent(t *testing.T) {
	t.Parallel()
	s := newUploadResumeSink(wiringLogger())
	scope := audit.ScopeID("upload.vol1.deadbeefdeadbeef")
	st := &uploadcore.ScopeState{UploadID: "u1", PartsDone: map[int]bool{0: true}}
	for i := range 3 {
		if err := s.PutResume(context.Background(), scope, st); err != nil {
			t.Fatalf("第 %d 次 PutResume: %v", i, err)
		}
	}
	if got := s.Len(); got != 1 {
		t.Fatalf("重复登记必须幂等：Len=%d want 1", got)
	}
	// 快照必须是深拷贝：外部改 map 不得影响内部（并发安全前提）。
	snap := s.Snapshot()
	snap[scope].PartsDone[1] = true
	if s.Snapshot()[scope].PartsDone[1] {
		t.Fatal("Snapshot 必须返回拷贝，不得暴露内部 map")
	}
	// nil 状态不得登记（防御：扫描实现返回 nil 表示无需续传）。
	if err := s.PutResume(context.Background(), audit.ScopeID("upload.vol1.cafe"), nil); err != nil {
		t.Fatalf("PutResume(nil): %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("nil 状态不得登记：Len=%d want 1", s.Len())
	}
}

// TestUploadLegacies_NilAuditSkips 无审计 ⇒ 不注册（避免启动恢复出现无意义的失败项）。
func TestUploadLegacies_NilAuditSkips(t *testing.T) {
	t.Parallel()
	sink := newUploadResumeSink(wiringLogger())
	if got := uploadLegacies(&Handlers{}, sink); len(got) != 0 {
		t.Fatalf("无 audit 时不应注册 legacy，got %d", len(got))
	}
	if got := uploadLegacies(nil, sink); len(got) != 0 {
		t.Fatalf("Handlers 为 nil 时不应注册，got %d", len(got))
	}
	if got := uploadLegacies(&Handlers{auditSvc: wiringAudit(t)}, nil); len(got) != 0 {
		t.Fatalf("sink 为 nil 时不应注册（否则恢复期 panic），got %d", len(got))
	}
}

// TestUploadLegacies_EndToEndRegistersUnfinishedUpload 端到端：未完成上传会话被登记且保持未完成。
func TestUploadLegacies_EndToEndRegistersUnfinishedUpload(t *testing.T) {
	t.Parallel()
	svc := wiringAudit(t)
	ctx := context.Background()
	// 走**内核真实写入路径**造 WAL：stage.upload（upload_id）+ part.0（已完成分片）。
	rec, err := uploadcore.OpenAuditRecorder(ctx, svc, "vol1", "dir/a.bin")
	if err != nil {
		t.Fatalf("OpenAuditRecorder: %v", err)
	}
	if err = rec.Stage(ctx, "upload", map[string]any{"upload_id": "u-42"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err = rec.PartDone(ctx, 0, "0cc175b9c0f1b6a831c399e269772661"); err != nil {
		t.Fatalf("PartDone: %v", err)
	}

	sink := newUploadResumeSink(wiringLogger())
	legs := uploadLegacies(&Handlers{auditSvc: svc}, sink)
	if len(legs) != 1 {
		t.Fatalf("装配了审计应注册 1 条 legacy，got %d", len(legs))
	}
	n, err := legs[0].Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover 应登记 1 项，got %d", n)
	}
	snap := sink.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("sink 应有 1 项，got %d", len(snap))
	}
	for scope, st := range snap {
		if want := uploadcore.UploadScopeID("vol1", "dir/a.bin"); scope != want {
			t.Fatalf("作用域应为确定性 id %s，got %s", want, scope)
		}
		if st.UploadID != "u-42" {
			t.Fatalf("upload_id 未恢复，got %q", st.UploadID)
		}
		if !st.PartsDone[0] {
			t.Fatalf("已完成分片未恢复，got %v", st.PartsDone)
		}
	}
	// **不 MarkDone**：作用域必须仍未完成 ⇒ 下次上传同一文件时才能续传。
	if st, err := uploadcore.ResumeScope(ctx, svc, "vol1", "dir/a.bin"); err != nil || st == nil {
		t.Fatalf("恢复后作用域必须保持未完成（续传前提）：st=%v err=%v", st, err)
	}
	// 重复扫描（协调器可能多次启动）：仍登记 1 项，不重复。
	if n, err := legs[0].Recover(ctx); err != nil || n != 1 {
		t.Fatalf("重复扫描应幂等：n=%d err=%v want n=1", n, err)
	}
	if sink.Len() != 1 {
		t.Fatalf("重复扫描不得重复登记：Len=%d want 1", sink.Len())
	}
}
