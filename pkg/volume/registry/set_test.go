// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// newTestSet 构造两卷（默认卷 default + disk2）的 Set：卷根是 t.TempDir() 下真实的
// storage.Root（OpenRoot 建目录并写/校验 LAYOUT_VERSION），容量池按传入上限建
// （0 = 不限量）。返回的 Set 由 t.Cleanup 统一 Close，避免 Windows 上句柄泄漏。
func newTestSet(t *testing.T, defaultCap, disk2Cap int64) *Set {
	t.Helper()
	defDir := t.TempDir()
	d2Dir := t.TempDir()
	defRoot, err := storage.OpenRoot(defDir)
	if err != nil {
		t.Fatalf("OpenRoot(默认卷根) 失败: %v", err)
	}
	d2Root, err := storage.OpenRoot(d2Dir)
	if err != nil {
		t.Fatalf("OpenRoot(disk2 根) 失败: %v", err)
	}
	set := NewSet(
		[]volume.Volume{
			{Name: "default", RootDir: defDir, Capacity: defaultCap},
			{Name: "disk2", RootDir: d2Dir, Capacity: disk2Cap},
		},
		map[string]*storage.Root{"default": defRoot, "disk2": d2Root},
		map[string]*quota.Pool{"default": quota.NewPool(defaultCap), "disk2": quota.NewPool(disk2Cap)},
		"default",
	)
	t.Cleanup(func() { _ = set.Close() })
	return set
}

// TestSet_Default_ReturnsFirstVolume 钉住「默认卷 = 声明序首卷」：Default() 与 defaultName
// 必须指向同一卷（装配层据此把 globalRoot 映射到默认卷根）。
func TestSet_Default_ReturnsFirstVolume(t *testing.T) {
	set := newTestSet(t, 100, 200)
	if got := set.Default().Name; got != "default" {
		t.Fatalf("Default().Name = %q, want %q", got, "default")
	}
	if set.defaultName != set.Default().Name {
		t.Fatalf("defaultName = %q 与 Default().Name = %q 不一致", set.defaultName, set.Default().Name)
	}
}

// TestSet_All_ReturnsCopy 钉住 All() 返回副本：改写返回切片不得污染 Set 内部底层数组。
func TestSet_All_ReturnsCopy(t *testing.T) {
	set := newTestSet(t, 100, 200)
	all := set.All()
	if len(all) != 2 {
		t.Fatalf("All() 长度 = %d, want 2", len(all))
	}
	all[0].Name = "被改写"
	if got := set.All()[0].Name; got != "default" {
		t.Fatalf("改写 All() 副本污染了内部状态：All()[0].Name = %q, want %q", got, "default")
	}
}

// TestSet_ByName 表驱动：命中返回卷描述，未命中返回零值 + false。
func TestSet_ByName(t *testing.T) {
	set := newTestSet(t, 100, 200)
	cases := []struct {
		name    string
		wantOK  bool
		wantCap int64
	}{
		{"default", true, 100},
		{"disk2", true, 200},
		{"不存在", false, 0},
		{"", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := set.ByName(tc.name)
			if ok != tc.wantOK {
				t.Fatalf("ByName(%q) ok = %v, want %v", tc.name, ok, tc.wantOK)
			}
			if ok && v.Capacity != tc.wantCap {
				t.Fatalf("ByName(%q).Capacity = %d, want %d", tc.name, v.Capacity, tc.wantCap)
			}
			if !ok && v.Name != "" {
				t.Fatalf("ByName(%q) 未命中应返回零值 Volume，got %+v", tc.name, v)
			}
		})
	}
}

// TestSet_RootAndDefaultRoot 钉住根句柄查询：DefaultRoot 走 defaultName，Root 按名查，
// 未知卷名一律 nil。
func TestSet_RootAndDefaultRoot(t *testing.T) {
	set := newTestSet(t, 100, 200)
	if set.DefaultRoot() == nil {
		t.Fatal("DefaultRoot() = nil, want 非 nil")
	}
	if set.DefaultRoot() != set.Root("default") {
		t.Fatal("DefaultRoot() 与 Root(defaultName) 应返回同一句柄")
	}
	if set.Root("不存在") != nil {
		t.Fatal("Root(未知卷名) 应返回 nil")
	}
}

// TestSet_Pool 钉住容量池查询：已知卷返回池（上限透传），未知卷返回 nil。
func TestSet_Pool(t *testing.T) {
	set := newTestSet(t, 100, 200)
	p := set.Pool("default")
	if p == nil {
		t.Fatal("Pool(default) = nil, want 非 nil")
	}
	if got := p.MaxBytes(); got != 100 {
		t.Fatalf("Pool(default).MaxBytes() = %d, want 100", got)
	}
	if set.Pool("不存在") != nil {
		t.Fatal("Pool(未知卷名) 应返回 nil")
	}
}

// TestSet_Tenant_LazyCreateAndCache 钉住租户懒建：首次调用在 <卷根>/<owner> 落目录并建租户，
// 再次调用返回同一实例（缓存命中，不重复 OpenRoot）。
func TestSet_Tenant_LazyCreateAndCache(t *testing.T) {
	set := newTestSet(t, 0, 0)
	first := set.Tenant("disk2", "alice", nil)
	if first == nil {
		t.Fatal("Tenant(disk2, alice) = nil, want 非 nil（合法 owner + 卷根可用）")
	}
	abs, ok := first.Root().Abs("")
	if !ok {
		t.Fatal("租户根的 Abs(\"\") 不可推导")
	}
	if _, err := os.Stat(filepath.Clean(abs)); err != nil {
		t.Fatalf("租户物理根 %q 未落盘: %v", abs, err)
	}
	if got := filepath.Base(filepath.Clean(abs)); got != "alice" {
		t.Fatalf("租户物理根末段 = %q, want %q（布局 <卷根>/<owner>）", got, "alice")
	}
	if second := set.Tenant("disk2", "alice", nil); second != first {
		t.Fatal("同一 (卷, owner) 二次调用应命中缓存返回同一实例")
	}
}

// assertVolumeRootUntouched 断言卷根目录下**除 OpenRoot 自建的 LAYOUT_VERSION 外没有任何条目**。
// 用于把「非法 owner fail-closed」从「返回 nil」钉到「磁盘零副作用」：只断返回值是不够的——
// 删掉 storage.ValidSegmentName 守卫后返回值**仍是 nil**（后续 NewTenant 同样拒绝），但
// MkdirAll + OpenRoot 早已在卷根留下 owner 目录。**该副作用只对 `owner=a/b` 这类能通过
// Root.Abs 越界检查的输入成立**（`""` 落回卷根自身不留新条目、`..` 被 Abs 直接拒绝，
// 两者与本守卫无关）；查盘才能测到这个差异。
func assertVolumeRootUntouched(t *testing.T, set *Set, volName string) {
	t.Helper()
	rt := set.Root(volName)
	if rt == nil {
		t.Fatalf("前置失败：卷 %q 无根句柄", volName)
	}
	rootDir, ok := rt.Abs("")
	if !ok {
		t.Fatalf("前置失败：卷 %q 的根绝对路径不可推导", volName)
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatalf("读取卷根 %q 失败: %v", rootDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// "LAYOUT_VERSION" 与 pkg/storage 的 layoutVersionFile 同名（该常量未导出，此处用字面量）。
	if len(names) != 1 || names[0] != "LAYOUT_VERSION" {
		t.Fatalf("卷根 %q 内容 = %v, want 仅 [LAYOUT_VERSION]（非法 owner 不得落任何目录/文件）", rootDir, names)
	}
}

// TestSet_Tenant_FailClosed 钉住 fail-closed：未知卷名、非法 owner 一律返回 nil，
// 且非法 owner 在卷根**零副作用**（同步查盘，见 assertVolumeRootUntouched）。
func TestSet_Tenant_FailClosed(t *testing.T) {
	set := newTestSet(t, 0, 0)
	if got := set.Tenant("不存在", "alice", nil); got != nil {
		t.Fatalf("Tenant(未知卷名) = %v, want nil", got)
	}
	for _, owner := range []string{"", "..", "a/b"} {
		t.Run("owner="+owner, func(t *testing.T) {
			if got := set.Tenant("disk2", owner, nil); got != nil {
				t.Fatalf("Tenant(disk2, %q) = %v, want nil（非法 owner 须 fail-closed）", owner, got)
			}
			// 同步断言：调用返回后立刻查盘（不留到用例末尾，避免被后续调用掩盖）。
			assertVolumeRootUntouched(t, set, "disk2")
		})
	}
}

// TestSet_Close_ClosesRootsAndIsIdempotent 钉住 Close 语义：关闭后根句柄查询为 nil（map 已清），
// 且重复调用不 panic（幂等）。
func TestSet_Close_ClosesRootsAndIsIdempotent(t *testing.T) {
	set := newTestSet(t, 0, 0)
	if tnt := set.Tenant("disk2", "bob", nil); tnt == nil {
		t.Fatal("前置失败：Tenant(disk2, bob) = nil")
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}
	if set.Root("default") != nil || set.Root("disk2") != nil {
		t.Fatal("Close() 后根句柄应被清空（map 内条目删除）")
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close() 二次调用应幂等，返回错误: %v", err)
	}
}
