// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

// consistency_test.go 验证联邦卷强一致性（roadmap 11.8-②）：
//  1. 默认（无 WithConflictMode）= lww：写面直转，零回归（无版本检查）。
//  2. version 模式：CheckVersion → WriteFileVersioned（CAS），匹配成功版本 +1；
//     不匹配 → VersionConflictError（409 + 当前版本号）。
//  3. conflict 模式：远端已存在 → 旧文件改名 .conflict-<ts> 保留 + 新内容写原路径；
//     不存在 → 无冲突直写。
//  4. version/conflict 模式 + 写面非 VersionedWriter → 装配 fail-fast（禁静默降级 lww）。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// mockVersionedFS 是带版本感知的写面 mock：模拟远端 CAS 语义（CheckVersion 读版本表；
// WriteFileVersioned 当前版本 == expected 才写并 +1，否则 VersionConflictError）。
// beforeCAS 在 CAS 比较前执行（测试注入，模拟并发写入者先到）。
type mockVersionedFS struct {
	*memWriterFS
	versions  map[string]int64 // path → 当前版本（缺省 = -1 不存在）
	checks    []string
	renames   [][2]string
	beforeCAS func()
}

func newMockVersionedFS() *mockVersionedFS {
	return &mockVersionedFS{
		memWriterFS: &memWriterFS{},
		versions:    map[string]int64{},
	}
}

func (m *mockVersionedFS) CheckVersion(_ context.Context, path string) (int64, error) {
	m.checks = append(m.checks, path)
	v, ok := m.versions[path]
	if !ok {
		return -1, nil
	}
	return v, nil
}

func (m *mockVersionedFS) WriteFileVersioned(_ context.Context, path string, _ io.Reader, _, _ int64, expected int64) error {
	if m.beforeCAS != nil {
		m.beforeCAS()
	}
	cur, ok := m.versions[path]
	if !ok {
		cur = -1
	}
	if cur != expected {
		return &VersionConflictError{Path: path, Current: cur}
	}
	m.versions[path] = cur + 1
	m.writeCalled = true
	return nil
}

func (m *mockVersionedFS) Rename(_ context.Context, from, to string) error {
	m.renames = append(m.renames, [2]string{from, to})
	if v, ok := m.versions[from]; ok {
		delete(m.versions, from)
		m.versions[to] = v
	}
	return nil
}

// TestFederated_LWW_ZeroRegression 默认（不调用 WithConflictMode）= lww：直转 Writer，
// 无版本检查（非 VersionedWriter 写面也可用——变异：默认改成 version → 红）。
func TestFederated_LWW_ZeroRegression(t *testing.T) {
	t.Parallel()
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := &memWriterFS{}
	fs = fs.WithWriter(w)
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("lww WriteFile = %v, want 直转成功", err)
	}
	if !w.writeCalled {
		t.Fatal("lww 应直转 Writer（writeCalled=false）")
	}
}

// TestFederated_VersionMatch_Writes version 模式 + 版本匹配 → 写入成功 + 版本 +1
// （变异：不检查直接写 → 红）。
func TestFederated_VersionMatch_Writes(t *testing.T) {
	t.Parallel()
	w := newMockVersionedFS()
	w.versions["a.txt"] = 3
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fs = fs.WithWriter(w).WithConflictMode(ConflictVersion)
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("v4"), 2, 0); err != nil {
		t.Fatalf("version 匹配 WriteFile = %v, want 成功", err)
	}
	if len(w.checks) != 1 || w.checks[0] != "a.txt" {
		t.Fatalf("CheckVersion 应被调用 1 次 a.txt, got %v", w.checks)
	}
	if got := w.versions["a.txt"]; got != 4 {
		t.Fatalf("写入后版本应 +1 → 4, got %d", got)
	}
	if !w.writeCalled {
		t.Fatal("版本匹配应真实写入（writeCalled=false）")
	}
}

// TestFederated_VersionMismatch_409 version 模式 + 版本不匹配（并发写入者先到）
// → VersionConflictError 携带当前版本号（变异：返回 200/静默覆盖 → 红）。
func TestFederated_VersionMismatch_409(t *testing.T) {
	t.Parallel()
	w := newMockVersionedFS()
	w.versions["a.txt"] = 5
	// 模拟并发写入者：CheckVersion 读到 5，WriteFileVersioned(expected=5) 前版本已变 6。
	w.beforeCAS = func() { w.versions["a.txt"] = 6 }
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fs = fs.WithWriter(w).WithConflictMode(ConflictVersion)
	err = fs.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 0)
	var vce *VersionConflictError
	if !errors.As(err, &vce) {
		t.Fatalf("版本不匹配应返回 VersionConflictError, got %v", err)
	}
	if vce.Path != "a.txt" {
		t.Fatalf("Path = %q, want a.txt", vce.Path)
	}
	if vce.Current != 6 {
		t.Fatalf("应携带当前版本 6（提示重取）, got %d", vce.Current)
	}
	if w.writeCalled {
		t.Fatal("CAS 失败不应真实写入")
	}
}

// TestFederated_Conflict_CreatesFile conflict 模式 + 远端已存在 → 旧文件改名
// .conflict-<ts> 保留 + 新内容写原路径（变异：不保留旧文件/不生成冲突文件 → 红）。
func TestFederated_Conflict_CreatesFile(t *testing.T) {
	t.Parallel()
	w := newMockVersionedFS()
	w.versions["a.txt"] = 1 // 已存在 → 写冲突
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fs = fs.WithWriter(w).WithConflictMode(ConflictConflict)
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("new"), 3, 0); err != nil {
		t.Fatalf("conflict WriteFile = %v, want 成功（旧文件保留 + 新内容写入）", err)
	}
	if len(w.renames) != 1 || w.renames[0][0] != "a.txt" {
		t.Fatalf("应 rename 旧文件 a.txt, got %v", w.renames)
	}
	if !strings.HasPrefix(w.renames[0][1], "a.txt.conflict-") {
		t.Fatalf("冲突文件应命名为 a.txt.conflict-*, got %q", w.renames[0][1])
	}
	if got := w.versions[w.renames[0][1]]; got != 1 {
		t.Fatalf("冲突文件应保留旧版本 1, got %d", got)
	}
	if got := w.versions["a.txt"]; got != 0 {
		t.Fatalf("原路径新内容版本应为 0, got %d", got)
	}
	if !w.writeCalled {
		t.Fatal("冲突处理应真实写入新内容")
	}
}

// TestFederated_Conflict_NoExisting_DirectWrite conflict 模式 + 远端不存在
// → 无冲突直写（无 rename，期望不存在写入）。
func TestFederated_Conflict_NoExisting_DirectWrite(t *testing.T) {
	t.Parallel()
	w := newMockVersionedFS()
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fs = fs.WithWriter(w).WithConflictMode(ConflictConflict)
	if err := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("conflict 模式不存在直写 = %v, want 成功", err)
	}
	if len(w.renames) != 0 {
		t.Fatalf("不存在不应 rename, got %v", w.renames)
	}
	if got := w.versions["a.txt"]; got != 0 {
		t.Fatalf("新写入版本应为 0, got %d", got)
	}
}

// TestFederated_UnsupportedWriter_FailFast Writer 非 VersionedWriter + version/conflict
// 模式 → 装配报错（fail-closed，禁静默降级 lww——变异：静默降级 → 红）。
func TestFederated_UnsupportedWriter_FailFast(t *testing.T) {
	t.Parallel()
	// 装配层（NewBackend）：version/conflict 模式 + 写面非 VersionedWriter → 装配报错。
	for _, mode := range []string{"version", "conflict"} {
		_, err := NewBackend(context.Background(), volume.Volume{
			Name: "fed1", Type: "federated-test",
			Extra: map[string]any{"node": "n1", "volume": "vol1", "writable": true, "conflict_mode": mode},
		}, testDialer())
		if err == nil {
			t.Fatalf("conflict_mode=%s 写面非 VersionedWriter 应装配报错（fail-closed）", mode)
		}
	}
	// 库层防御：直接 WithConflictMode(version) + 非 VersionedWriter 写面 → WriteFile 报错。
	fs, err := New(memReader{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fs = fs.WithWriter(&memWriterFS{}).WithConflictMode(ConflictVersion)
	if werr := fs.WriteFile(context.Background(), "a.txt", strings.NewReader("x"), 1, 0); !errors.Is(werr, ErrVersionedUnsupported) {
		t.Fatalf("非 VersionedWriter + version 模式 WriteFile = %v, want ErrVersionedUnsupported", werr)
	}
}

// TestFederated_ConflictMode_Parse conflict_mode 解析：lww/version/conflict（大小写
// 不敏感）+ 非法值报错。
func TestFederated_ConflictMode_Parse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want ConflictMode
	}{
		{"", ConflictLWW},
		{"lww", ConflictLWW},
		{"LWW", ConflictLWW},
		{"version", ConflictVersion},
		{"conflict", ConflictConflict},
	}
	for _, c := range cases {
		got, err := ParseConflictMode(c.in)
		if err != nil || got != c.want {
			t.Fatalf("ParseConflictMode(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := ParseConflictMode("bogus"); err == nil {
		t.Fatal("非法 conflict_mode 应报错")
	}
}

// TestFederated_NewBackend_ConflictModeLWW 装配层：conflict_mode=lww 显式配置 →
// 装配成功（零回归，不要求 VersionedWriter）。
func TestFederated_NewBackend_ConflictModeLWW(t *testing.T) {
	t.Parallel()
	be, err := NewBackend(context.Background(), volume.Volume{
		Name: "fed1", Type: "federated-test",
		Extra: map[string]any{"node": "n1", "volume": "vol1", "writable": true, "conflict_mode": "lww"},
	}, testDialer())
	if err != nil {
		t.Fatalf("conflict_mode=lww 装配应成功: %v", err)
	}
	defer be.Close()
}
