// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVerify_Consistent 验证校验通过：created/updated 目标文件 checksum 与源一致，
// 结果不追加 verify_failed。
func TestVerify_Consistent(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{Src: "", Dst: "", Recursive: true}
	engine := &Engine{}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 校验后不得追加 verify_failed 结果。
	failures, err := Verify(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("期望 0 校验失败，got %d: %+v", len(failures), failures)
	}
}

// TestVerify_Mismatch 验证校验失败检测：目标内容被篡改后 Verify 应报告 verify_failed。
func TestVerify_Mismatch(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{Src: "", Dst: "", Recursive: true}
	engine := &Engine{}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 篡改目标（模拟传输/落盘损坏）。
	if err := os.WriteFile(filepath.Join(dstRoot, "a.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures, err := Verify(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("期望 1 校验失败，got %d: %+v", len(failures), failures)
	}
	if failures[0].Action != ActionVerifyFailed {
		t.Fatalf("期望 ActionVerifyFailed，got %q", failures[0].Action)
	}
	if failures[0].Path != "a.txt" {
		t.Fatalf("期望校验失败路径 a.txt，got %q", failures[0].Path)
	}
}

// TestVerify_DefaultOff_ZeroRegression 验证默认关闭零回归：job.VerifyAfter=false 时
// Sync 后 Results 不含 verify_failed（校验不执行，零开销）。
func TestVerify_DefaultOff_ZeroRegression(t *testing.T) {
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := &Job{Src: "", Dst: "", Recursive: true}
	engine := &Engine{}
	if err := engine.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// 默认关：篡改目标后 Results 也不应出现 verify_failed（Verify 未执行）。
	if err := os.WriteFile(filepath.Join(dstRoot, "a.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range job.Results {
		if r.Action == ActionVerifyFailed {
			t.Fatalf("默认关闭却出现 verify_failed 结果: %+v", r)
		}
	}
}

// TestSummary_CountsByAction 验证汇总统计：按 Action 分组计数。
func TestSummary_CountsByAction(t *testing.T) {
	t.Parallel()
	results := []FileResult{
		{Path: "a.txt", Action: ActionCreated},
		{Path: "b.txt", Action: ActionCreated},
		{Path: "c.txt", Action: ActionSkipped},
		{Path: "d.txt", Action: ActionError, Error: "boom"},
		{Path: "e.txt", Action: ActionVerifyFailed},
	}
	s := SummaryOf(results)
	if s.Created != 2 || s.Updated != 0 || s.Skipped != 1 || s.Errors != 1 || s.VerifyFailed != 1 {
		t.Fatalf("Summary 计数不符: %+v", s)
	}
}
