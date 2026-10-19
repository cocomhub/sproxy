// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// wal_test.go 锁定 M2（设计 docs/designs/2026-10-19-audit-service.md §6）：
// ① Resume **按 Seq 重放**且纯依赖 Journal（进程重启后仍可用）；
// ② **提交语义红灯用例**：MarkDone 之前崩溃 ⇒ 重放完成；MarkDone 之后 ⇒ **跳过**（设计 §6.1）；
// ③ 幂等契约：apply 出错必须向上传播（不静默跳过状态）；
// ④ ListScopes(unfinished) 正确区分已提交/未完成。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// replayRec 记录一次 Apply 调用。
type replayRec struct {
	seq  uint64
	rec  StateRecord
	call int
}

// recordingApplier 记录重放顺序与载荷（测试替身）。
type recordingApplier struct {
	calls []replayRec
	fail  error
}

func (a *recordingApplier) Apply(_ context.Context, _ ScopeID, seq uint64, state []byte) error {
	if a.fail != nil {
		return a.fail
	}
	var rec StateRecord
	if err := json.Unmarshal(state, &rec); err != nil {
		return err
	}
	a.calls = append(a.calls, replayRec{seq: seq, rec: rec, call: len(a.calls) + 1})
	return nil
}

// newWALService 建一个 FileJournal 支撑的 Service（写入真落盘 ⇒ 可模拟重启）。
func newWALService(t *testing.T) (*Service, *FileJournal) {
	t.Helper()
	j, err := NewFileJournal(FileJournalConfig{
		Dir:    filepath.Join(t.TempDir(), "wal"),
		Layout: LayoutPerScope,
		// 任务/WAL 段族：**不配置 MaxTotalBytes**（设计 E27：任务族永不丢弃）。
		SyncMode: SyncAlways,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	// Windows 下未关闭的文件句柄会阻止 TempDir 清理 ⇒ 测试结束统一关闭（幂等）。
	t.Cleanup(func() { _ = j.Close() })
	caps := FullCapabilities()
	s, err := New(Options{Journal: j, Caps: &caps, Clock: systemClock{}, NodeID: "node-a"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, j
}

// TestResume_ReplaysInSeqOrder 重启后按 Seq 升序重放（纯 Journal 驱动）。
func TestResume_ReplaysInSeqOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, j := newWALService(t)
	sc, err := s.Open(ctx, "task-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, kv := range [][2]any{{"step", "download"}, {"progress", 50}, {"progress", 80}} {
		if serr := sc.Save(ctx, kv[0].(string), kv[1]); serr != nil {
			t.Fatalf("Save: %v", serr)
		}
	}
	// 模拟进程重启：丢弃内存态，直接用同一 Journal 建新 Service。
	s2, err := New(Options{Journal: j, Clock: systemClock{}, NodeID: "node-a"})
	if err != nil {
		t.Fatalf("New(restart): %v", err)
	}
	ap := &recordingApplier{}
	st, rerr := s2.Resume(ctx, "task-1", ap)
	if rerr != nil {
		t.Fatalf("Resume: %v", rerr)
	}
	if st.Done {
		t.Fatal("未 MarkDone 的作用域不应判为已完成")
	}
	if len(ap.calls) != 3 {
		t.Fatalf("应重放 3 条 KindState, got %d", len(ap.calls))
	}
	for i := 1; i < len(ap.calls); i++ {
		if ap.calls[i].seq <= ap.calls[i-1].seq {
			t.Fatalf("重放必须按 Seq 升序: %d then %d", ap.calls[i-1].seq, ap.calls[i].seq)
		}
	}
	if st.States != 3 {
		t.Fatalf("ScopeState.States = %d, want 3", st.States)
	}
	if got := st.KV["progress"]; got != float64(80) {
		t.Fatalf("KV 应按 Seq 覆盖（progress=80）, got %v", got)
	}
	if st.LastSeq != ap.calls[len(ap.calls)-1].seq {
		t.Fatalf("LastSeq 应为最后一条 Seq: %d vs %d", st.LastSeq, ap.calls[len(ap.calls)-1].seq)
	}
}

// TestResume_SkipsAfterMarkDone 提交语义红灯用例（设计 §6.1）：
// MarkDone 之前 ⇒ 重放；MarkDone 之后 ⇒ **跳过**（否则会重复执行已完成的副作用）。
func TestResume_SkipsAfterMarkDone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, j := newWALService(t)
	sc, err := s.Open(ctx, "task-2")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if serr := sc.Save(ctx, "k", "v"); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	// ① MarkDone 之前：崩溃 ⇒ 重启后必须重放。
	s2, _ := New(Options{Journal: j, Clock: systemClock{}})
	ap := &recordingApplier{}
	st, rerr := s2.Resume(ctx, "task-2", ap)
	if rerr != nil {
		t.Fatalf("Resume(before done): %v", rerr)
	}
	if st.Done || len(ap.calls) != 1 {
		t.Fatalf("MarkDone 前应重放 1 条: done=%v calls=%d", st.Done, len(ap.calls))
	}
	// ② 业务副作用完成 ⇒ MarkDone（提交记录）。
	if merr := sc.MarkDone(ctx); merr != nil {
		t.Fatalf("MarkDone: %v", merr)
	}
	// ③ MarkDone 之后：崩溃 ⇒ 重启后必须**跳过**（绝不重复执行副作用）。
	s3, _ := New(Options{Journal: j, Clock: systemClock{}})
	ap2 := &recordingApplier{}
	st3, rerr := s3.Resume(ctx, "task-2", ap2)
	if rerr != nil {
		t.Fatalf("Resume(after done): %v", rerr)
	}
	if !st3.Done {
		t.Fatal("MarkDone 之后 Done 必须为 true")
	}
	if len(ap2.calls) != 0 {
		t.Fatalf("已提交作用域不得重放（否则副作用重复执行）, got %d 次", len(ap2.calls))
	}
}

// TestResume_ApplierErrorPropagates apply 失败必须向上传播（不得静默跳过状态）。
func TestResume_ApplierErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newWALService(t)
	sc, _ := s.Open(ctx, "task-3")
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	ap := &recordingApplier{fail: context.DeadlineExceeded}
	if _, err := s.Resume(ctx, "task-3", ap); err == nil {
		t.Fatal("apply 失败应返回错误（不静默跳过状态）")
	}
}

// TestListScopes_UnfinishedOnly 恢复协调器的主用入口：只列未完成作用域。
func TestListScopes_UnfinishedOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newWALService(t)
	for _, id := range []ScopeID{"task-a", "task-b", "task-c"} {
		sc, err := s.Open(ctx, id)
		if err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		if serr := sc.Save(ctx, "k", string(id)); serr != nil {
			t.Fatalf("Save: %v", serr)
		}
		if id == "task-b" {
			if merr := sc.MarkDone(ctx); merr != nil {
				t.Fatalf("MarkDone: %v", merr)
			}
		}
	}
	all, err := s.ListScopes(ctx, ScopeStateFilter{})
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("应有 3 个作用域, got %d", len(all))
	}
	unfinished, err := s.ListScopes(ctx, ScopeStateFilter{UnfinishedOnly: true})
	if err != nil {
		t.Fatalf("ListScopes(unfinished): %v", err)
	}
	if len(unfinished) != 2 {
		t.Fatalf("未完成应为 2 个, got %d: %+v", len(unfinished), unfinished)
	}
	for _, info := range unfinished {
		if info.Scope == "task-b" {
			t.Fatalf("已 MarkDone 的作用域不应出现在 unfinished: %+v", info)
		}
		if info.LastSeq == 0 || info.States != 1 {
			t.Fatalf("摘要字段不完整: %+v", info)
		}
	}
	// Limit（最近优先）与 Since 过滤。
	one, err := s.ListScopes(ctx, ScopeStateFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListScopes(limit): %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("Limit=1 应只返回 1 条, got %d", len(one))
	}
	if _, err := s.ListScopes(ctx, ScopeStateFilter{Since: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("ListScopes(since): %v", err)
	}
}

// TestMarkDone_ServiceLevelWritesCommitRecord Service 级 MarkDone 等价于 Scope 级（写 KindDone）。
func TestMarkDone_ServiceLevelWritesCommitRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, j := newWALService(t)
	sc, _ := s.Open(ctx, "task-9")
	if serr := sc.Save(ctx, "k", "v"); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	if err := s.MarkDone(ctx, "task-9"); err != nil {
		t.Fatalf("Service.MarkDone: %v", err)
	}
	entries, err := j.Read(EntryFilter{Scope: "task-9"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) == 0 || entries[len(entries)-1].Kind != KindDone {
		t.Fatalf("提交记录应落在末尾且为 KindDone: %+v", entries)
	}
	s2, _ := New(Options{Journal: j, Clock: systemClock{}})
	st, rerr := s2.Resume(ctx, "task-9", &recordingApplier{})
	if rerr != nil || !st.Done {
		t.Fatalf("Service.MarkDone 之后 Resume 应判已完成: done=%v err=%v", st.Done, rerr)
	}
}
