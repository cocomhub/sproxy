// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issues

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileStore_RoundTripAndAtomic 锁定文件存储契约：
// 不存在 ⇒ 空且不报错；写入后可读回；**目录可不存在（自动创建）**；写坏不能留半截。
func TestFileStore_RoundTripAndAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "issues.json")
	st := NewFileStore(p)

	// 文件不存在 ⇒ 空列表 + 无错（首次启动的正常路径）
	got, err := st.Load()
	if err != nil || len(got) != 0 {
		t.Fatalf("首次 Load = (%v,%v)，期望 (空,nil)", got, err)
	}

	in := []Item{
		{ID: "b:2", Kind: KindUploadFailed, Volume: "v", Path: "p2", Status: StatusOpen},
		{ID: "a:1", Kind: KindDirMetaAmbiguous, Volume: "v", Path: "p1", Status: StatusNeedsHuman,
			Detail: "两个 dir-meta，需人工仲裁", LastSeen: time.Unix(100, 0)},
	}
	if serr := st.Save(in); serr != nil {
		t.Fatalf("Save: %v", serr)
	}
	out, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("读回 %d 条，期望 2", len(out))
	}
	// 内容稳定（按 ID 排序，便于人工审阅/diff）
	if out[0].ID != "a:1" || out[1].ID != "b:2" {
		t.Fatalf("应稳定按 ID 排序：%s, %s", out[0].ID, out[1].ID)
	}
	// 必须保留**详细原因**（人工判断依据）
	if out[0].Detail != "两个 dir-meta，需人工仲裁" {
		t.Fatalf("Detail 丢失: %q", out[0].Detail)
	}
	// 无残留临时文件（原子替换后已清理）
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("存在残留临时文件: %s", e.Name())
		}
	}
}

// TestFileStore_LoadCorruptReturnsError 锁定：损坏文件**不得**静默当作空列表
// （否则审计数据"消失"会被误认为正常；应报错 → 由 Manager 登记为可感知事项）。
func TestFileStore_LoadCorruptReturnsError(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "issues.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := NewFileStore(p).Load(); err == nil {
		t.Fatal("损坏文件应返回错误（不得静默当空）")
	}
}

// TestManager_FileStorePersistence 端到端锁定：Manager + FileStore 跨实例可续（重启后仍可见且可续跑）。
func TestManager_FileStorePersistence(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	p := filepath.Join(t.TempDir(), "issues.json")

	m1 := New(Options{Clock: clk.Now, Store: NewFileStore(p), RetryBackoff: time.Minute})
	m1.Report(Item{Kind: KindDirMetaCleanupFailed, Volume: "baidu", Path: "c1", Detail: "删旧 meta 失败", Retryable: true})
	clk.Advance(2 * time.Minute)
	m1.RunDue(ctx1(t), clk.now, map[Kind]RetryFunc{
		KindDirMetaCleanupFailed: func(context.Context, Item) error { return nil },
	})

	m2 := New(Options{Clock: clk.Now, Store: NewFileStore(p)})
	got := m2.List(ListFilter{})
	if len(got) != 1 {
		t.Fatalf("重启后应保留 1 条，got %d", len(got))
	}
	if got[0].Detail != "删旧 meta 失败" {
		t.Fatalf("Detail 应保留：%q", got[0].Detail)
	}
}
