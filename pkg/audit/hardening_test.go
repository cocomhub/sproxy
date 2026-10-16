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

// hardening_test.go 钉住审计包的**安全与正确性契约**（代码审核 2026-10-15 修复项）：
//
//   - S2：scopeID 白名单（拒绝路径穿越与特殊文件名）；
//   - S1：sink 目录/文件拒绝符号链接、目录必须私有（审计是证据，被他人读/改即失效）；
//   - C1：Recent 不得因超长行静默丢掉文件剩余内容（旧 Scanner 会以 ErrTooLong 停止）；
//   - C2：Filter.Limit 上限语义；
//   - C3：With 纯派生（无副作用）+ Set 写共享 kvStore；
//   - C4：Span.End/Fail 幂等（重复收口不得重复落行）；
//   - C5：未知步骤回落通用类型 TypeLog（不得谎报 TypeResource），TypeError 命中；
//   - C6：bytes/bw_bps 不可数值化时保留原值进 Meta（不得静默写 0）。
//
// 变异：① 去掉 scopeID 白名单 ⇒ 第一条红；② 去掉符号链接校验 ⇒ 第二/三条红；
// ③ Recent 改回 Scanner ⇒ 超长行用例红；④ End 去掉 done 门 ⇒ 幂等用例红。

// newSinkForTest 建一个合法 sink（临时目录 + 合法 scopeID）。
func newSinkForTest(t *testing.T, scopeID string) (*FileSink, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewScopeSink(dir, scopeID)
	if err != nil {
		t.Fatalf("NewScopeSink(%q): %v", scopeID, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// TestNewScopeSink_RejectsUnsafeScopeID：scopeID 直接参与文件名拼接 ⇒ 必须白名单校验。
func TestNewScopeSink_RejectsUnsafeScopeID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, id := range []string{
		"", "..", "../evil", "../../etc/passwd", "a/b", `a\b`,
		".hidden", "-lead", "x\x00y", "a b", strings.Repeat("a", 129),
	} {
		if _, err := NewScopeSink(dir, id); err == nil {
			t.Errorf("scopeID %q 应被拒绝（路径穿越/非法字符/超长）", id)
		}
	}
	// 合法形态：字母/数字开头 + [A-Za-z0-9._-]。
	for _, id := range []string{"abc", "task-1.2_3", "A1"} {
		s, err := NewScopeSink(dir, id)
		if err != nil {
			t.Fatalf("scopeID %q 应合法: %v", id, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// TestNewScopeSink_RejectsSymlinkedDir：目录为符号链接 ⇒ 拒绝（防审计写到他人指定位置）。
func TestNewScopeSink_RejectsSymlinkedDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("平台不支持符号链接: %v", err)
	}
	if _, err := NewScopeSink(link, "task1"); err == nil {
		t.Fatal("目录是符号链接时必须拒绝（防审计重定向）")
	}
}

// TestNewScopeSink_RejectsSymlinkedFile：目标文件为符号链接 ⇒ 拒绝（Unix 由 O_NOFOLLOW
// 原子保证，其他平台打开前 Lstat 校验）。
func TestNewScopeSink_RejectsSymlinkedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "task1.audit.log")); err != nil {
		t.Skipf("平台不支持符号链接: %v", err)
	}
	if _, err := NewScopeSink(dir, "task1"); err == nil {
		t.Fatal("目标文件是符号链接时必须拒绝（防审计写入重定向）")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Fatalf("被链接的目标文件不得被写入: %q err=%v", string(b), err)
	}
}

// TestNewScopeSink_TightensNonPrivateDir：既有目录含 group/other 位 ⇒ **自愈收紧为 0700**
// （安全目标是「审计目录最终私有」，而非让审计在常见环境（用户缓存目录/t.TempDir()）整体不可用）。
// 仅 Unix 断言权限位：Windows 的 Mode().Perm() 不反映 ACL。
func TestNewScopeSink_TightensNonPrivateDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 Mode().Perm() 不反映 ACL")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	s, err := NewScopeSink(dir, "task1")
	if err != nil {
		t.Fatalf("非私有既有目录应被收紧而非拒绝: %v", err)
	}
	_ = s.Close()
	fi, serr := os.Lstat(dir)
	if serr != nil {
		t.Fatalf("Lstat: %v", serr)
	}
	if !privatePermOK(fi.Mode().Perm()) {
		t.Fatalf("目录权限应被收紧为私有（无 group/other 位），got %04o", fi.Mode().Perm())
	}
}

// TestPrivatePermOK 跨平台钉住私有判据（判据与平台无关；平台分支只决定是否调用它）。
func TestPrivatePermOK(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		perm os.FileMode
		want bool
	}{
		{0o700, true}, {0o600, true}, {0o500, true},
		{0o750, false}, {0o705, false}, {0o777, false}, {0o770, false},
	} {
		if got := privatePermOK(tc.perm); got != tc.want {
			t.Errorf("privatePermOK(%04o) = %v, want %v", tc.perm, got, tc.want)
		}
	}
}

// TestFileSink_Recent_LongLineNotTruncated：超长行（>旧 Scanner 4MiB 上限）之后的
// 行必须仍然可读——否则查询结果**静默缺尾**（看起来像「审计没记」）。
func TestFileSink_Recent_LongLineNotTruncated(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	big := strings.Repeat("x", 5<<20)
	for _, r := range []Row{
		{Type: TypeLog, Level: LevelInfo, Step: "before"},
		{Type: TypeLog, Level: LevelInfo, Step: "big", Meta: map[string]any{"pad": big}},
		{Type: TypeLog, Level: LevelInfo, Step: "after"},
	} {
		if err := s.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rows := s.Recent(Filter{})
	if len(rows) != 3 {
		t.Fatalf("应返回 3 行（超长行不得截断后续），got %d", len(rows))
	}
	if rows[2].Step != "after" {
		t.Fatalf("末行应为 after，got %q", rows[2].Step)
	}
}

// TestFileSink_Recent_Limit：Limit > 0 时最多返回 Limit 条；0 = 不限。
func TestFileSink_Recent_Limit(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	for range 3 {
		if err := s.Append(Row{Type: TypeLog, Level: LevelInfo, Step: "s"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := len(s.Recent(Filter{Limit: 2})); got != 2 {
		t.Fatalf("Limit=2 ⇒ 2 条, got %d", got)
	}
	if got := len(s.Recent(Filter{})); got != 3 {
		t.Fatalf("Limit=0 ⇒ 全部 3 条, got %d", got)
	}
}

// TestLogger_WithIsPureAndSetWritesKV：With 只换维度（无副作用）；键值写入走 Set。
func TestLogger_WithIsPureAndSetWritesKV(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	l, err := NewScope("task1", ScopeOpts{Sink: s})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	ctx := context.Background()
	sys := l.With(DimSystem)
	if _, ok := l.Get(ctx, "k"); ok {
		t.Fatal("With 不得产生副作用（键值写入走 Set）")
	}
	sys.Set("k", "v")
	if v, ok := l.Get(ctx, "k"); !ok || v != "v" {
		t.Fatalf("Set 应写共享 kvStore, got (%v,%v)", v, ok)
	}
	// 维度隔离：派生 logger 的维度生效，原 logger 不变。
	sys.Log(TypeLog, LevelInfo, "s")
	rows := s.Recent(Filter{})
	if len(rows) != 1 || rows[0].Dim != DimSystem {
		t.Fatalf("派生 logger 应使用新维度, got %+v", rows)
	}
}

// TestSpan_EndAndFail_Idempotent：重复收口不得重复落行（否则同一操作多行且耗时自相矛盾）。
func TestSpan_EndAndFail_Idempotent(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	l, err := NewScope("task1", ScopeOpts{Sink: s})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	sp := l.Begin("download")
	sp.End()
	sp.End()
	sp.Fail(errors.New("late"))
	if got := len(s.Recent(Filter{})); got != 1 {
		t.Fatalf("重复收口应只落 1 行, got %d", got)
	}
}

// TestBegin_TypeFallback：未知步骤回落通用类型 TypeLog（不得谎报 TypeResource 采样）；
// step 命中已知类型值（含 error）时用之。
func TestBegin_TypeFallback(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	l, err := NewScope("task1", ScopeOpts{Sink: s})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	for _, tc := range []struct {
		step string
		want Type
	}{
		{"integrity", TypeLog},
		{"anything-else", TypeLog},
		{"error", TypeError},
		{"download", TypeDownload},
		{"encrypt", TypeEncrypt},
		{"resource", TypeResource},
	} {
		l.Begin(tc.step).End()
	}
	rows := s.Recent(Filter{})
	if len(rows) != 6 {
		t.Fatalf("应落 6 行, got %d", len(rows))
	}
	for i, tc := range []string{"integrity", "anything-else", "error", "download", "encrypt", "resource"} {
		want := []Type{TypeLog, TypeLog, TypeError, TypeDownload, TypeEncrypt, TypeResource}[i]
		if rows[i].Step != tc || rows[i].Type != want {
			t.Errorf("step %q: type=%q want %q", tc, rows[i].Type, want)
		}
	}
}

// TestSpan_End_NonNumericBytesKeepsRaw：bytes/bw_bps 不可数值化时保留原值进 Meta，
// 不得静默写 0（审计里「0 字节」是有效语义，类型写错记成 0 会污染数据）。
func TestSpan_End_NonNumericBytesKeepsRaw(t *testing.T) {
	t.Parallel()
	s, _ := newSinkForTest(t, "task1")
	l, err := NewScope("task1", ScopeOpts{Sink: s})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	l.Begin("encrypt").End("bytes", "12x", "bw_bps", "fast")
	rows := s.Recent(Filter{})
	if len(rows) != 1 {
		t.Fatalf("应落 1 行, got %d", len(rows))
	}
	if rows[0].Bytes != 0 || rows[0].BandwidthBps != 0 {
		t.Fatalf("不可数值化 ⇒ 字段保持 0, got bytes=%d bw=%d", rows[0].Bytes, rows[0].BandwidthBps)
	}
	m, _ := rows[0].Meta.(map[string]any)
	if m["bytes"] != "12x" || m["bw_bps"] != "fast" {
		t.Fatalf("原值必须保留进 Meta（不得静默丢），got %+v", m)
	}
}
