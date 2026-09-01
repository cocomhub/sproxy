// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"testing"
)

// newTestTenant 创建临时目录 + Root + Tenant 的测试辅助。
func newTestTenant(t *testing.T, owner string) *Tenant {
	t.Helper()
	r, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	tnt, err := NewTenant(owner, r)
	if err != nil {
		t.Fatal(err)
	}
	return tnt
}

// TestTenant_UserRel Tenant 布局与 rel 判定。
func TestTenant_UserRel(t *testing.T) {
	tnt := newTestTenant(t, "alice")
	rel, ok := tnt.UserRel("dir/report.pdf")
	if !ok || rel != "user/dir/report.pdf" {
		t.Fatalf("UserRel=%q,%v", rel, ok)
	}
	if _, ok := tnt.UserRel("../etc"); ok {
		t.Fatalf("穿越应拒绝")
	}
	if _, ok := tnt.UserRel("__version__/x"); ok {
		t.Fatalf("非 user 桶前缀输入应拒绝")
	}
	if _, ok := tnt.UserRel("cloud/x"); ok {
		t.Fatalf("用户不得触碰功能桶")
	}
	if rel, ok := tnt.FeatureRel("cloud", "t1/f.bin"); !ok || rel != "cloud/t1/f.bin" {
		t.Fatalf("FeatureRel=%q,%v", rel, ok)
	}
	if _, ok := tnt.FeatureRel("bogus", "x"); ok {
		t.Fatalf("未知桶应拒绝")
	}
}

// TestTenant_UserRel_Extra 补充 UserRel 边界：反斜杠/空/绝对/空段/保留段/深层合法。
func TestTenant_UserRel_Extra(t *testing.T) {
	tnt := newTestTenant(t, "alice")
	cases := []struct {
		in  string
		ok  bool
		rel string
	}{
		{"report.pdf", true, "user/report.pdf"},
		{`dir\file.txt`, true, "user/dir/file.txt"}, // 反斜杠视为分隔符
		{"", false, ""},
		{"   ", false, ""},
		{"/abs", false, ""},
		{`\abs`, false, ""},
		{"a//b", false, ""},
		{"./x", false, ""},
		{"a/../b", false, ""},
		{"CON.txt", false, ""},                    // 保留设备名段
		{"meta/x", false, ""},                     // 功能桶名首段且带子路径拒绝
		{"user/x", false, ""},                     // user 桶名首段且带子路径拒绝
		{"__x/y", false, ""},                      // __ 遗留前缀首段且带子路径拒绝
		{"cloud", true, "user/cloud"},             // 单段功能桶名是用户合法命名（user/ 前缀物理隔离）
		{"archive", true, "user/archive"},         // 同上
		{"meta", true, "user/meta"},               // 同上
		{"dir/cloud/x", true, "user/dir/cloud/x"}, // 深层功能桶名合法（物理隔离保证）
		{"dir/__x", true, "user/dir/__x"},         // 深层 __ 前缀合法
	}
	for _, c := range cases {
		rel, ok := tnt.UserRel(c.in)
		if ok != c.ok || (ok && rel != c.rel) {
			t.Errorf("UserRel(%q)=%q,%v want %q,%v", c.in, rel, ok, c.rel, c.ok)
		}
	}
}

// TestTenant_NewTenant 验证 owner 段名校验 fail-closed 与 nil root 拒绝。
func TestTenant_NewTenant(t *testing.T) {
	r, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	tnt, err := NewTenant("alice", r)
	if err != nil {
		t.Fatalf("合法 owner 应成功: %v", err)
	}
	if tnt.ID != "alice" {
		t.Fatalf("ID=%q", tnt.ID)
	}
	if tnt.Root() != r {
		t.Fatalf("Root() 应返回同一 root")
	}

	for _, bad := range []string{"", ".", "..", "a/b", "CON", "foo.", ".__x__", "a:b", `x\y`} {
		if _, err := NewTenant(bad, r); err == nil {
			t.Fatalf("非法 owner %q 应失败", bad)
		}
	}
	if _, err := NewTenant("alice", nil); err == nil {
		t.Fatalf("nil root 应失败")
	}
}

// TestTenant_BucketsAndUserRoot 验证用户桶名与功能桶白名单。
func TestTenant_BucketsAndUserRoot(t *testing.T) {
	tnt := newTestTenant(t, "alice")
	if got := tnt.UserRoot(); got != "user" {
		t.Fatalf("UserRoot=%q", got)
	}
	want := []string{"user", "cloud", "archive", "chunk", "version", "meta"}
	got := tnt.Buckets()
	if len(got) != len(want) {
		t.Fatalf("Buckets=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Buckets[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

// TestTenant_FeatureRel_Extra 补充 FeatureRel 边界：空 sub / 白名单桶 / 穿越 sub。
func TestTenant_FeatureRel_Extra(t *testing.T) {
	tnt := newTestTenant(t, "alice")
	if rel, ok := tnt.FeatureRel("cloud", ""); !ok || rel != "cloud" {
		t.Fatalf("FeatureRel(cloud, 空)=%q,%v", rel, ok)
	}
	for _, b := range []string{"user", "cloud", "archive", "chunk", "version", "meta"} {
		if _, ok := tnt.FeatureRel(b, "x"); !ok {
			t.Fatalf("桶 %q 应在白名单内", b)
		}
	}
	if _, ok := tnt.FeatureRel("bogus", "x"); ok {
		t.Fatalf("未知桶应拒绝")
	}
	if _, ok := tnt.FeatureRel("cloud", "../x"); ok {
		t.Fatalf("FeatureRel 拒绝穿越 sub")
	}
	if _, ok := tnt.FeatureRel("cloud", "/abs"); ok {
		t.Fatalf("FeatureRel 拒绝绝对 sub")
	}
	if _, ok := tnt.FeatureRel("cloud", "a:b/x"); ok {
		t.Fatalf("FeatureRel 拒绝 Windows 非法字符段")
	}
	if _, ok := tnt.FeatureRel("user", "CON.txt"); ok {
		t.Fatalf("FeatureRel 拒绝保留设备名段")
	}
}

// TestNormalizeRemote 路径归一：/ 分隔符、反斜杠转义、拒绝空/绝对/.. /./空段。
func TestNormalizeRemote(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		out string
	}{
		{"a/b/c", true, "a/b/c"},
		{`a\b\c`, true, "a/b/c"},
		{"report.pdf", true, "report.pdf"},
		{"", false, ""},
		{"   ", false, ""},
		{"/abs", false, ""},
		{`\abs`, false, ""},
		{"a/../b", false, ""},
		{"../b", false, ""},
		{"./b", false, ""},
		{"a//b", false, ""},
		{"a/./b", false, ""},
		{"C:/x", false, ""},
		{`C:\x`, false, ""},
	}
	for _, c := range cases {
		out, ok := NormalizeRemote(c.in)
		if ok != c.ok || (ok && out != c.out) {
			t.Errorf("NormalizeRemote(%q)=%q,%v want %q,%v", c.in, out, ok, c.out, c.ok)
		}
	}
}

// TestJoinRel 协议路径拼接。
func TestJoinRel(t *testing.T) {
	if got := JoinRel("user", "dir", "f.txt"); got != "user/dir/f.txt" {
		t.Fatalf("JoinRel=%q", got)
	}
	if got := JoinRel(); got != "" {
		t.Fatalf("JoinRel()=%q", got)
	}
	if got := JoinRel("cloud", "t1"); got != "cloud/t1" {
		t.Fatalf("JoinRel=%q", got)
	}
}
