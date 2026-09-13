// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// openTestRoot 打开一个临时目录作为存储根（测试基座）。
func openTestRoot(t *testing.T) *Root {
	t.Helper()
	rt, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot 失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func TestNormalizeOwner(t *testing.T) {
	if got := NormalizeOwner(""); got != AnonymousOwner {
		t.Fatalf("空 owner 应归一为 %q，got %q", AnonymousOwner, got)
	}
	if AnonymousOwner != "anonymous" {
		t.Fatalf("AnonymousOwner 是存储布局契约（磁盘目录名），不得变更：%q", AnonymousOwner)
	}
	if got := NormalizeOwner("alice"); got != "alice" {
		t.Fatalf("非空 owner 应原样返回，got %q", got)
	}
}

func TestOpenTenant_Success(t *testing.T) {
	parent := openTestRoot(t)
	tn, err := OpenTenant(parent, "alice")
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	t.Cleanup(func() { _ = tn.Root().Close() })
	if tn.ID != "alice" {
		t.Fatalf("租户 ID 应为 alice，got %q", tn.ID)
	}
	abs, ok := parent.Abs("alice")
	if !ok {
		t.Fatal("parent.Abs(alice) 失败")
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		t.Fatalf("租户目录未被创建: err=%v", err)
	}
	// 未开 WithMetaBucket：meta 桶不得存在（默认卷才预建）。
	if _, err := os.Stat(filepath.Join(abs, "meta")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("未开 WithMetaBucket 时 meta 桶不应存在: err=%v", err)
	}
}

func TestOpenTenant_MetaBucketOption(t *testing.T) {
	parent := openTestRoot(t)
	tn, err := OpenTenant(parent, "alice", WithMetaBucket())
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	t.Cleanup(func() { _ = tn.Root().Close() })
	abs, _ := parent.Abs("alice")
	if fi, err := os.Stat(filepath.Join(abs, "meta")); err != nil || !fi.IsDir() {
		t.Fatalf("WithMetaBucket 应预建 meta 桶: err=%v", err)
	}
}

func TestOpenTenant_FailClosed(t *testing.T) {
	parent := openTestRoot(t)
	for _, owner := range []string{"", ".", "..", "a/b", `a\b`, ".__x", "CON", "name.", "name "} {
		tn, err := OpenTenant(parent, owner)
		if err == nil || tn != nil {
			t.Fatalf("非法 owner %q 应 (nil, err)，got (%v, %v)", owner, tn, err)
		}
	}
	// parent 未装配：fail-closed，绝不回落。
	if tn, err := OpenTenant(nil, "alice"); err == nil || tn != nil {
		t.Fatalf("parent==nil 应 (nil, err)，got (%v, %v)", tn, err)
	}
	// 非法 owner 不得在存储根下留下任何目录。
	if got := ListOwners(parent); len(got) != 0 {
		t.Fatalf("非法 owner 不应创建目录，ListOwners=%v", got)
	}
}

// TestOpenTenant_NoHandleLeak 用「能否删除目录」作为句柄是否泄漏的可执行探针：
// Windows 上未关闭的 os.Root 句柄会让 RemoveAll 失败。
func TestOpenTenant_NoHandleLeak(t *testing.T) {
	dir := t.TempDir()
	parent, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot 失败: %v", err)
	}
	defer func() { _ = parent.Close() }()

	tn, err := OpenTenant(parent, "alice", WithMetaBucket())
	if err != nil {
		t.Fatalf("OpenTenant 失败: %v", err)
	}
	if err := tn.Root().Close(); err != nil {
		t.Fatalf("关闭租户根失败: %v", err)
	}
	if err := parent.Close(); err != nil {
		t.Fatalf("关闭父根失败: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("租户根目录无法删除（句柄泄漏？）: %v", err)
	}
}

func TestListOwners(t *testing.T) {
	parent := openTestRoot(t)
	for _, name := range []string{"bob", "alice"} {
		tn, err := OpenTenant(parent, name)
		if err != nil {
			t.Fatalf("OpenTenant(%s) 失败: %v", name, err)
		}
		t.Cleanup(func() { _ = tn.Root().Close() })
	}
	// 干扰项：文件、遗留内部目录（__ 前缀）、非法段名目录（Windows 非法字符），都不算出。
	base, _ := parent.Abs("")
	for _, name := range []string{"__legacydir", "bad:name"} {
		_ = os.MkdirAll(filepath.Join(base, name), 0o755)
	}
	if err := os.WriteFile(filepath.Join(base, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写干扰文件失败: %v", err)
	}

	got := ListOwners(parent)
	want := []string{"alice", "bob"}
	if len(got) != len(want) {
		t.Fatalf("ListOwners 应只返回合法租户目录且按名排序，got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListOwners[%d]=%q want %q（完整结果 %v）", i, got[i], want[i], got)
		}
	}
	if ListOwners(nil) != nil {
		t.Fatal("parent==nil 时 ListOwners 应返回 nil")
	}
}

func TestTenantCache_SuccessAndReuse(t *testing.T) {
	parent := openTestRoot(t)
	c := NewTenantCache(parent, WithMetaBucket())
	t.Cleanup(func() { _ = c.Close() })
	first := c.TenantFor("alice")
	if first == nil {
		t.Fatal("TenantFor 应返回租户")
	}
	if second := c.TenantFor("alice"); second != first {
		t.Fatal("同一 owner 应复用缓存中的同一个租户（句柄不得重复打开）")
	}
	// 空 owner 不归一（策略在调用方）：本类型按非法 owner fail-closed。
	if c.TenantFor("") != nil {
		t.Fatal("空 owner 应返回 nil（归一由调用方负责）")
	}
	// 显式 anonymous 正常。
	if anon := c.TenantFor(AnonymousOwner); anon == nil || anon.ID != AnonymousOwner {
		t.Fatalf("TenantFor(anonymous) = %v, want 租户", anon)
	}
	if c.TenantFor("..") != nil {
		t.Fatal("非法 owner 应返回 nil（fail-closed）")
	}
	if NewTenantCache(nil).TenantFor("alice") != nil {
		t.Fatal("parent==nil 时 TenantFor 应返回 nil")
	}
}

func TestTenantCache_Concurrent(t *testing.T) {
	parent := openTestRoot(t)
	c := NewTenantCache(parent, WithMetaBucket())
	t.Cleanup(func() { _ = c.Close() })
	var wg sync.WaitGroup
	results := make([]*Tenant, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.TenantFor("alice")
		}(i)
	}
	wg.Wait()
	for i, tn := range results {
		if tn == nil {
			t.Fatalf("并发 TenantFor[%d] 返回 nil", i)
		}
		if tn != results[0] {
			t.Fatalf("并发 TenantFor[%d] 与 [0] 不是同一实例（缓存未生效）", i)
		}
	}
}

func TestTenantCache_CloseKeepsParentUsable(t *testing.T) {
	parent := openTestRoot(t)
	c := NewTenantCache(parent, WithMetaBucket())
	defer func() { _ = c.Close() }()
	if c.TenantFor("alice") == nil {
		t.Fatal("TenantFor 应返回租户")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// Close 幂等。
	if err := c.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	// 关键：Close 不得关闭 parent（parent 由创建者负责）。
	if _, ok := parent.Abs("alice"); !ok {
		t.Fatal("Close 后 parent 应仍可用")
	}
	// Close 后缓存清空 → 可重建。
	if c.TenantFor("alice") == nil {
		t.Fatal("Close 后 TenantFor 应能重建租户")
	}
	// nil 接收者安全（装配层可能持有 nil 缓存）。
	var nilCache *TenantCache
	if nilCache.TenantFor("alice") != nil {
		t.Fatal("nil 缓存应返回 nil")
	}
	if err := nilCache.Close(); err != nil {
		t.Fatalf("nil 缓存 Close 应返回 nil: %v", err)
	}
}
