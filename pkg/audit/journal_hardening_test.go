// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// journal_hardening_test.go 移植并覆盖旧 FileSink 的**安全不变量**（审计是证据 ⇒ 不可被
// 重定向/篡改/读取）：符号链接拒绝、非私有目录自愈、长行不截断；以及步骤语义（幂等、
// Fail ⇒ LevelError、BeginSpan nil-safe）。与旧 hardening_test.go 同一契约，测新实现。

func TestFileJournal_RejectsSymlinkedDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需特权（与旧 hardening 测试同口径）")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("无法创建符号链接（跳过）：%v", err)
	}
	if _, err := NewFileJournal(FileJournalConfig{Dir: link, Layout: LayoutGlobal}); err == nil {
		t.Fatalf("符号链接目录必须被拒绝（防审计写入被重定向到他人可读/可改位置）")
	}
}

func TestFileJournal_RejectsSymlinkedFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接需特权（与旧 hardening 测试同口径）")
	}
	dir := t.TempDir()
	evil := filepath.Join(t.TempDir(), "evil.log")
	if err := os.WriteFile(evil, []byte("victim\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(evil, filepath.Join(dir, "compliance.audit.log")); err != nil {
		t.Skipf("无法创建符号链接（跳过）：%v", err)
	}
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()
	if aerr := j.Append(mkEntry("s", "a", "alice", 1)); aerr == nil {
		t.Fatalf("段文件是符号链接时必须拒绝写入（O_NOFOLLOW/Lstat 契约）")
	}
	data, _ := os.ReadFile(evil)
	if strings.Contains(string(data), `"scope"`) {
		t.Fatalf("审计行不得被写入符号链接目标（证据被重定向）")
	}
}

func TestFileJournal_TightensNonPrivateDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 Mode().Perm() 不反映 ACL")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("非私有目录应被自愈（chmod 0700）而非拒绝：%v", err)
	}
	defer j.Close()
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("目录权限 = %o, want 700（自愈失败）", st.Mode().Perm())
	}
}

func TestFileJournal_LongLineNotTruncated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()
	// 超长行（> bufio.Scanner 的 64KB 上限）：用 Scanner 实现会**静默丢掉文件剩余内容**。
	long := strings.Repeat("x", 200*1024)
	if aerr := j.Append(Entry{Scope: "s", Seq: 1, Kind: KindEvent, Action: "a", Detail: long}); aerr != nil {
		t.Fatalf("Append(long): %v", aerr)
	}
	if aerr := j.Append(Entry{Scope: "s", Seq: 2, Kind: KindEvent, Action: "after"}); aerr != nil {
		t.Fatalf("Append(after): %v", aerr)
	}
	got, err := j.Read(EntryFilter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("超长行后仍应读到后续行（不得截断）：got %d", len(got))
	}
	if len(got[0].Detail) != len(long) {
		t.Fatalf("超长行内容被截断：%d != %d", len(got[0].Detail), len(long))
	}
	if got[1].Action != "after" {
		t.Fatalf("后续行内容错误：%+v", got[1])
	}
}

func TestService_StepIdempotentAndErrLevel(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	sc, _ := s.Open(ctx, "s")

	st := s.Begin(ctx, "step-a")
	st.End()
	st.End() // 幂等：只写一行
	spans, _ := j.Read(EntryFilter{Kind: KindSpan})
	if len(spans) != 1 {
		t.Fatalf("End 幂等失败：span 数 = %d, want 1", len(spans))
	}
	if spans[0].Level != LevelInfo {
		t.Fatalf("成功 span Level = %q, want info", spans[0].Level)
	}

	// Fail ⇒ LevelError + Err + 仍只一行；Fail 后再 End 不得再写。
	st2 := s.Begin(ctx, "step-b")
	st2.Fail(errors.New("boom"), "bytes", 7)
	st2.End()
	spans, _ = j.Read(EntryFilter{Kind: KindSpan})
	if len(spans) != 2 {
		t.Fatalf("Fail 后 End 不得重复写入：span 数 = %d, want 2", len(spans))
	}
	failed := spans[1]
	if failed.Level != LevelError || failed.Err != "boom" {
		t.Fatalf("Fail 语义错误：level=%q err=%q", failed.Level, failed.Err)
	}
	if failed.Bytes != 7 {
		t.Fatalf("Fail 的 kv 未写入：bytes=%d", failed.Bytes)
	}
	_ = sc
}

func TestService_BeginSpanNilSafe(t *testing.T) {
	t.Parallel()
	// 无服务 ⇒ nil，且 nil 上的 End/Fail 必须安全（等价旧 From(ctx).Begin 的链式安全）。
	sp := BeginSpan(context.Background(), "noop")
	if sp != nil {
		t.Fatalf("无服务时 BeginSpan 应返回 nil")
	}
	sp.End()
	sp.Fail(errors.New("x"))

	// 注入服务 ⇒ 正常写入（且 ctx 中取用等价）。
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := WithService(context.Background(), s)
	if got := ServiceFrom(ctx); got != s {
		t.Fatalf("ServiceFrom 未取回注入的服务")
	}
	sp2 := BeginSpan(ctx, "wired", "bytes", 3)
	if sp2 == nil {
		t.Fatalf("注入服务后 BeginSpan 不应为 nil")
	}
	sp2.End()
	spans, _ := j.Read(EntryFilter{Kind: KindSpan})
	if len(spans) != 1 || spans[0].Step != "wired" || spans[0].Bytes != 3 {
		t.Fatalf("注入路径 span 写入错误：%+v", spans)
	}
	// 能力关闭 ⇒ BeginSpan 返回 nil（不写）。
	tele := TelemetryOnly()
	_ = tele
	comp := ComplianceOnly()
	s2, err := New(Options{Journal: NewMemoryJournal(), Caps: &comp, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New(2): %v", err)
	}
	if sp3 := BeginSpan(WithService(context.Background(), s2), "x"); sp3 != nil {
		t.Fatalf("合规剖面未启用 span ⇒ BeginSpan 应返回 nil")
	}
}
