// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// seq_restart_test.go 锁定 **Seq 跨进程单调**（2026-10-20 修 P0）。
//
// 缺陷：`Service.seq` 是内存 map ⇒ 进程重启后归零 ⇒ 重启后首条写入拿到 `Seq=1`。
// 而恢复路径以「该作用域**最大 Seq** 的条目为末条」判定是否已提交 ⇒ 重启后写的 `KindDone`
// （提交记录）Seq 比历史状态更小 ⇒ **被忽略** ⇒ 作用域永远「未完成」⇒ 恢复**每轮启动都重复
// 执行同一任务**（副作用重复，正是设计 §6.1 明令禁止的失败模式）。
//
// 修复：每个作用域在本进程首次写入前，从 Journal 取已知最大 Seq 作为起点（只升不降）。

import (
	"context"
	"path/filepath"
	"testing"
)

// seqRestartService 在指定目录上建「进程」：journal + service（调用方负责 Close）。
func seqRestartService(t *testing.T, dir string) (*Service, *FileJournal) {
	t.Helper()
	j, err := NewFileJournal(FileJournalConfig{
		Dir: dir, Layout: LayoutPerScope, SyncMode: SyncAlways,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	caps := FullCapabilities()
	s, err := New(Options{Journal: j, Caps: &caps})
	if err != nil {
		_ = j.Close()
		t.Fatalf("New: %v", err)
	}
	return s, j
}

// TestSeqContinuesAcrossProcessRestart 重启后（同目录新进程）写入的 Seq 必须接着历史最大值。
func TestSeqContinuesAcrossProcessRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "wal")

	// 进程 A：两条状态（Seq 1、2）。
	svcA, jA := seqRestartService(t, dir)
	scA, err := svcA.Open(ctx, "task.cloud.restart")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, v := range []int{1, 2} {
		if serr := scA.Save(ctx, "step", v); serr != nil {
			t.Fatalf("Save: %v", serr)
		}
	}
	if err := jA.Close(); err != nil { // 模拟进程退出（关闭是可选的：崩溃也可）
		t.Fatalf("Close A: %v", err)
	}

	// 进程 B（重启）：写提交记录 ⇒ Seq 必须是 3（而不是 1）。
	svcB, jB := seqRestartService(t, dir)
	defer jB.Close()
	if merr := svcB.MarkDone(ctx, "task.cloud.restart"); merr != nil {
		t.Fatalf("MarkDone: %v", merr)
	}
	entries, rerr := jB.Read(EntryFilter{Scope: "task.cloud.restart"})
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if len(entries) != 3 {
		t.Fatalf("应有 3 条（2 state + 1 done）, got %d", len(entries))
	}
	last := entries[len(entries)-1]
	if last.Kind != KindDone {
		t.Fatalf("末条应为 KindDone, got %s", last.Kind)
	}
	if last.Seq != 3 {
		t.Fatalf("重启后续号失败：KindDone.Seq=%d，应为 3（若为 1 则恢复会永远判未完成 ⇒ 无限重复执行）", last.Seq)
	}

	// 读面必须看到「已提交且末条 Seq=3」。
	infos, lerr := svcB.ListScopes(ctx, ScopeStateFilter{})
	if lerr != nil {
		t.Fatalf("ListScopes: %v", lerr)
	}
	if len(infos) != 1 {
		t.Fatalf("应有 1 个作用域, got %d", len(infos))
	}
	if !infos[0].Done {
		t.Fatalf("重启后提交记录必须被识别为已提交（Done=false ⇒ 恢复会重复执行）: %+v", infos[0])
	}
	if infos[0].LastSeq != 3 {
		t.Fatalf("LastSeq 应为 3, got %d", infos[0].LastSeq)
	}
	if infos[0].States != 2 {
		t.Fatalf("States 应为 2, got %d", infos[0].States)
	}

	// 未完成集合应为空（不再无限重复恢复）；Resume 亦判已完成且零重放。
	unfinished, uerr := svcB.ListScopes(ctx, ScopeStateFilter{UnfinishedOnly: true})
	if uerr != nil {
		t.Fatalf("ListScopes(unfinished): %v", uerr)
	}
	if len(unfinished) != 0 {
		t.Fatalf("重启后续号修复后不应残留未完成作用域: %+v", unfinished)
	}
	st, serr := svcB.Resume(ctx, "task.cloud.restart", nil)
	if serr != nil {
		t.Fatalf("Resume: %v", serr)
	}
	if !st.Done || st.States != 0 {
		t.Fatalf("Resume 应判已完成且零重放: done=%v states=%d", st.Done, st.States)
	}
}

// TestSeqContinuesWithoutRestart 同进程内序遍历不变（续号逻辑不破坏正常路径）。
func TestSeqContinuesWithoutRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, j := seqRestartService(t, filepath.Join(t.TempDir(), "wal"))
	defer j.Close()
	sc, err := svc.Open(ctx, "task.cloud.sameproc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if serr := sc.Save(ctx, "a", 1); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	if merr := svc.MarkDone(ctx, "task.cloud.sameproc"); merr != nil {
		t.Fatalf("MarkDone: %v", merr)
	}
	entries, _ := j.Read(EntryFilter{Scope: "task.cloud.sameproc"})
	if len(entries) != 2 || entries[1].Seq != 2 {
		t.Fatalf("同进程内 Seq 应为 1、2; got %+v", entries)
	}
}
