// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package uploadcore

// recovery_test.go —— S3：验证「启动恢复只枚举待续传并交给 sink，**不 MarkDone**」。
// 这是与 recovery.Register（重放后立即 MarkDone）的**关键语义差异**：上传续传由业务路径
// 触发，若启动即 MarkDone 会丢掉待续传状态。

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/recovery"
)

// memSink 是内存版 ResumeSink（幂等：同 scope 覆盖）。
type memSink struct {
	mu    sync.Mutex
	items map[audit.ScopeID]*ScopeState
}

func newMemSink() *memSink { return &memSink{items: map[audit.ScopeID]*ScopeState{}} }

func (s *memSink) PutResume(_ context.Context, id audit.ScopeID, st *ScopeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = st
	return nil
}

func (s *memSink) get(id audit.ScopeID) *ScopeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[id]
}

func (s *memSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// TestLegacyRecovery_EnumeratesWithoutMarkDone —— 核心：扫描到待续传 ⇒ 交给 sink，且
// **不写提交记录**（作用域保持未完成）⇒ 可重复扫描（幂等）且不丢状态。
func TestLegacyRecovery_EnumeratesWithoutMarkDone(t *testing.T) {
	t.Parallel()
	svc := newAuditService(t)
	ctx := context.Background()

	// 造一个"中断的上传"：upload_id + 2 个已完成分片（未 MarkDone）。
	rec, oErr := OpenAuditRecorder(ctx, svc, "mydisk", "d/a.bin")
	if oErr != nil {
		t.Fatalf("OpenAuditRecorder: %v", oErr)
	}
	if err := rec.Stage(ctx, walStageUpload, map[string]any{"upload_id": "up-3"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	for _, seq := range []int{0, 2} {
		if err := rec.PartDone(ctx, seq, ""); err != nil {
			t.Fatalf("PartDone: %v", err)
		}
	}

	// 另造一个**别的任务族**的作用域：本入口必须不消费它。
	other, oErr := svc.Open(ctx, audit.ScopeID("cloud.otherscope"))
	if oErr != nil {
		t.Fatalf("open other scope: %v", oErr)
	}
	if sErr := other.Save(ctx, "x", 1); sErr != nil {
		t.Fatalf("other.Save: %v", sErr)
	}

	sink := newMemSink()
	coord := recovery.New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := coord.RegisterLegacy(NewLegacyRecovery(svc, sink)); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}
	rep, err := coord.Run(ctx, recovery.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.LegacyRecovered != 1 {
		t.Fatalf("应恢复 1 条待续传，got %d（report=%s）", rep.LegacyRecovered, rep)
	}
	if sink.len() != 1 {
		t.Fatalf("sink 应收到 1 条，got %d", sink.len())
	}
	got := sink.get(UploadScopeID("mydisk", "d/a.bin"))
	if got == nil || got.UploadID != "up-3" {
		t.Fatalf("恢复态应含 uploadID=up-3，got %+v", got)
	}
	if len(got.PartsDone) != 2 || !got.PartsDone[0] || !got.PartsDone[2] {
		t.Fatalf("恢复态应含分片 {0,2}，got %+v", got.PartsDone)
	}

	// **关键断言**：未 MarkDone ⇒ 再次 Run 仍能扫描到（不丢状态；sink 幂等覆盖）。
	if _, err := coord.Run(ctx, recovery.Options{}); err != nil {
		t.Fatalf("第二次 Run: %v", err)
	}
	if again := sink.get(UploadScopeID("mydisk", "d/a.bin")); again == nil || again.UploadID != "up-3" {
		t.Fatalf("第二次扫描后状态仍在（不得因恢复而被清除），got %+v", again)
	}
	if sink.len() != 1 {
		t.Fatalf("sink 应保持 1 条（幂等），got %d", sink.len())
	}
}

// TestLegacyRecovery_SkipsDoneScope 已完成（MarkDone）的作用域不再被扫描。
func TestLegacyRecovery_SkipsDoneScope(t *testing.T) {
	t.Parallel()
	svc := newAuditService(t)
	ctx := context.Background()
	rec, oErr := OpenAuditRecorder(ctx, svc, "mydisk", "d/done.bin")
	if oErr != nil {
		t.Fatalf("OpenAuditRecorder: %v", oErr)
	}
	if err := rec.Stage(ctx, walStageUpload, map[string]any{"upload_id": "up-done"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := rec.MarkDone(ctx); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	sink := newMemSink()
	coord := recovery.New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := coord.RegisterLegacy(NewLegacyRecovery(svc, sink)); err != nil {
		t.Fatalf("RegisterLegacy: %v", err)
	}
	if _, err := coord.Run(ctx, recovery.Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.len() != 0 {
		t.Fatalf("已完成作用域不应进入待续传，got %d", sink.len())
	}
}

// TestLegacyRecovery_Guards 缺参数必须显式报错（不得 panic）。
func TestLegacyRecovery_Guards(t *testing.T) {
	t.Parallel()
	if _, err := NewLegacyRecovery(nil, newMemSink()).Recover(context.Background()); err == nil {
		t.Fatal("nil service 应报错")
	}
	svc := newAuditService(t)
	if _, err := NewLegacyRecovery(svc, nil).Recover(context.Background()); err == nil {
		t.Fatal("nil sink 应报错")
	}
}
