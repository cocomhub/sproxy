// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

// router_test.go 锁 pkg/volume.Router 的不变量（P1-a 小步：只新增能力，不接线）：
//   - 同 rel 至多命中一卷：多卷命中时默认卷优先，其余按视图顺序首命中胜出；
//   - ACL 排除 fail-closed：默认卷/视图被拒时，文件即使物理存在也不得命中；
//   - 未装配（deps 全 nil）/非法 owner fail-closed（不泄卷存在性）；
//   - Locate 只读：探测不创建任何目录；
//   - 空 owner 归一为 anonymous（storage.NormalizeOwner 单一权威）。

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// routerRel 是测试用的域内路径（含桶前缀，与 locateOwnerFile 入参口径一致）。
const routerRel = "user/dir/file.bin"

// routerTenant 在真实 storage 根上建租户（定位经真实 os.Root.Stat）。
func routerTenant(t *testing.T, owner string) *storage.Tenant {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("storage.OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tnt, err := storage.NewTenant(owner, root)
	if err != nil {
		t.Fatalf("storage.NewTenant(%q): %v", owner, err)
	}
	return tnt
}

// routerPut 在租户根下写入 rel（含父目录），模拟卷上已有文件。
func routerPut(t *testing.T, tnt *storage.Tenant, rel string) {
	t.Helper()
	if dir := path.Dir(rel); dir != "." {
		if err := tnt.Root().MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	f, err := tnt.Root().OpenFile(rel, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", rel, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", rel, err)
	}
}

// routerEntries 返回租户根目录的条目名快照（判定 Locate 是否有副作用）。
func routerEntries(t *testing.T, tnt *storage.Tenant) []string {
	t.Helper()
	ents, err := os.ReadDir(tnt.Root().AbsPath())
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", tnt.Root().AbsPath(), err)
	}
	names := make([]string, len(ents))
	for i := range ents {
		names[i] = ents[i].Name()
	}
	return names
}

// routerViews 构造「固定视图」依赖（各用例按需覆盖）。
func routerViews(views ...RouterView) func(string) []RouterView {
	return func(string) []RouterView { return views }
}

// routerAlways / routerNever 是 ACL 判定的固定桩。
func routerAlways(string) bool { return true }
func routerNever(string) bool  { return false }

// TestRouterLocate_DefaultVolumePreferred：同 rel 在默认卷与视图都存在 → 默认卷优先。
func TestRouterLocate_DefaultVolumePreferred(t *testing.T) {
	t.Parallel()
	def := routerTenant(t, "alice")
	view := routerTenant(t, "alice")
	routerPut(t, def, routerRel)
	routerPut(t, view, routerRel)

	r := NewRouter(RouterDeps{
		DefaultTenant:  func(string) *storage.Tenant { return def },
		DefaultAllowed: routerAlways,
		Views:          routerViews(RouterView{Name: "disk2", Tenant: view, Allowed: true}),
	})
	loc, ok := r.Locate("alice", routerRel)
	if !ok {
		t.Fatalf("Locate 未命中：默认卷有文件应命中")
	}
	if loc.Volume != "" {
		t.Errorf("默认卷优先：Volume = %q，期望空串（默认卷）", loc.Volume)
	}
	if loc.Tenant != def {
		t.Errorf("默认卷优先：Tenant 非默认卷租户")
	}
}

// TestRouterLocate_FirstViewHitWins：默认卷未命中时，多视图同 rel → 视图顺序首命中胜出，
// 且只返回一个位置（调用方只应依赖「至多一卷命中」）。
func TestRouterLocate_FirstViewHitWins(t *testing.T) {
	t.Parallel()
	def := routerTenant(t, "alice")
	first := routerTenant(t, "alice")
	second := routerTenant(t, "alice")
	routerPut(t, first, routerRel)
	routerPut(t, second, routerRel)

	r := NewRouter(RouterDeps{
		DefaultTenant:  func(string) *storage.Tenant { return def },
		DefaultAllowed: routerAlways,
		Views: routerViews(
			RouterView{Name: "disk2", Tenant: first, Allowed: true},
			RouterView{Name: "disk3", Tenant: second, Allowed: true},
		),
	})
	loc, ok := r.Locate("alice", routerRel)
	if !ok {
		t.Fatalf("Locate 未命中：视图 disk2 有文件应命中")
	}
	if loc.Volume != "disk2" || loc.Tenant != first {
		t.Errorf("视图顺序首命中胜出：got volume=%q，期望 disk2", loc.Volume)
	}
}

// TestRouterLocate_ACLAndAssemblyFailClosed：ACL 排除 / 未装配 / 租户不可用一律未命中；
// 默认卷被拒时仍可命中 ACL 放行的视图（默认卷拒绝不等于全视图拒绝）。
func TestRouterLocate_ACLAndAssemblyFailClosed(t *testing.T) {
	t.Parallel()
	tnt := routerTenant(t, "alice")
	routerPut(t, tnt, routerRel) // 文件物理存在——fail-closed 仍必须未命中

	defTnt := func(string) *storage.Tenant { return tnt }

	cases := []struct {
		name    string
		deps    RouterDeps
		wantOK  bool
		wantVol string
	}{
		{
			name: "未装配",
			deps: RouterDeps{},
		},
		{
			name: "默认卷 ACL 拒绝且无视图",
			deps: RouterDeps{DefaultTenant: defTnt, DefaultAllowed: routerNever},
		},
		{
			name: "默认卷 ACL 拒绝但视图放行",
			deps: RouterDeps{
				DefaultTenant:  defTnt,
				DefaultAllowed: routerNever,
				Views:          routerViews(RouterView{Name: "disk2", Tenant: tnt, Allowed: true}),
			},
			wantOK:  true,
			wantVol: "disk2",
		},
		{
			name: "默认卷放行但租户不可用",
			deps: RouterDeps{DefaultAllowed: routerAlways},
		},
		{
			name: "视图 ACL 拒绝",
			deps: RouterDeps{Views: routerViews(RouterView{Name: "disk2", Tenant: tnt, Allowed: false})},
		},
		{
			name: "视图租户不可用",
			deps: RouterDeps{Views: routerViews(RouterView{Name: "disk2", Allowed: true})},
		},
		{
			name: "视图名缺失",
			deps: RouterDeps{Views: routerViews(RouterView{Tenant: tnt, Allowed: true})},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			loc, ok := NewRouter(tc.deps).Locate("alice", routerRel)
			if ok != tc.wantOK {
				t.Fatalf("Locate ok = %v，期望 %v（loc=%+v）", ok, tc.wantOK, loc)
			}
			if ok && loc.Volume != tc.wantVol {
				t.Errorf("Locate Volume = %q，期望 %q", loc.Volume, tc.wantVol)
			}
		})
	}
}

// TestRouterLocate_IllegalOwnerFailClosed：非法 owner（逃逸段 / 保留功能桶名）一律未命中
// 且 ACL 拒绝——即使 deps 对任意 owner 都返回可用租户。
func TestRouterLocate_IllegalOwnerFailClosed(t *testing.T) {
	t.Parallel()
	tnt := routerTenant(t, "alice")
	routerPut(t, tnt, routerRel)
	r := NewRouter(RouterDeps{
		DefaultTenant:  func(string) *storage.Tenant { return tnt },
		DefaultAllowed: routerAlways,
		Views:          routerViews(RouterView{Name: "disk2", Tenant: tnt, Allowed: true}),
	})
	for _, owner := range []string{".", "..", "a/b", `a\b`, "user", "META", "con"} {
		if loc, ok := r.Locate(owner, routerRel); ok {
			t.Errorf("非法 owner %q 应未命中，got %+v", owner, loc)
		}
		if r.Allowed(owner, "") || r.Allowed(owner, "disk2") {
			t.Errorf("非法 owner %q 应 ACL 拒绝", owner)
		}
	}
}

// TestRouterLocate_ReadOnlyNoSideEffects：探测未命中不得创建任何目录（只读定位）。
func TestRouterLocate_ReadOnlyNoSideEffects(t *testing.T) {
	t.Parallel()
	def := routerTenant(t, "alice")
	view := routerTenant(t, "alice")
	beforeDef := routerEntries(t, def)
	beforeView := routerEntries(t, view)

	r := NewRouter(RouterDeps{
		DefaultTenant:  func(string) *storage.Tenant { return def },
		DefaultAllowed: routerAlways,
		Views:          routerViews(RouterView{Name: "disk2", Tenant: view, Allowed: true}),
	})
	missing := "user/missing/deep/file.bin"
	if loc, ok := r.Locate("alice", missing); ok {
		t.Fatalf("不存在的 rel 应未命中，got %+v", loc)
	}
	if got := routerEntries(t, def); !slices.Equal(got, beforeDef) {
		t.Errorf("默认卷租户根被改动：before=%v after=%v", beforeDef, got)
	}
	if got := routerEntries(t, view); !slices.Equal(got, beforeView) {
		t.Errorf("视图租户根被改动：before=%v after=%v", beforeView, got)
	}
	for _, tnt := range []*storage.Tenant{def, view} {
		dir := filepath.Join(tnt.Root().AbsPath(), "user")
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("探测创建了目录 %q（err=%v）", dir, err)
		}
	}
}

// TestRouterLocate_NormalizesOwner：空 owner 归一为 anonymous（storage.NormalizeOwner）。
func TestRouterLocate_NormalizesOwner(t *testing.T) {
	t.Parallel()
	tnt := routerTenant(t, storage.AnonymousOwner)
	routerPut(t, tnt, routerRel)
	var seen []string
	r := NewRouter(RouterDeps{
		DefaultAllowed: func(owner string) bool {
			seen = append(seen, owner)
			return true
		},
		DefaultTenant: func(owner string) *storage.Tenant {
			seen = append(seen, owner)
			return tnt
		},
	})
	if loc, ok := r.Locate("", routerRel); !ok {
		t.Fatalf("空 owner 应归一为 %q 并命中，got %+v", storage.AnonymousOwner, loc)
	}
	if len(seen) == 0 {
		t.Fatalf("deps 未被调用")
	}
	for _, owner := range seen {
		if owner != storage.AnonymousOwner {
			t.Errorf("deps 收到 owner %q，期望归一为 %q", owner, storage.AnonymousOwner)
		}
	}
}

// TestRouterAllowed：卷 ACL 判定（默认卷 / 视图 / 未知卷 / 未装配）fail-closed。
func TestRouterAllowed(t *testing.T) {
	t.Parallel()
	tnt := routerTenant(t, "alice")
	r := NewRouter(RouterDeps{
		DefaultTenant:  func(string) *storage.Tenant { return tnt },
		DefaultAllowed: func(owner string) bool { return owner == "alice" },
		Views: routerViews(
			RouterView{Name: "disk2", Tenant: tnt, Allowed: true},
			RouterView{Name: "vault", Allowed: false},
		),
	})
	cases := []struct {
		name  string
		owner string
		vol   string
		want  bool
	}{
		{name: "默认卷放行", owner: "alice", vol: "", want: true},
		{name: "默认卷拒绝", owner: "bob", vol: "", want: false},
		{name: "视图放行", owner: "bob", vol: "disk2", want: true},
		{name: "视图拒绝", owner: "alice", vol: "vault", want: false},
		{name: "未知卷名", owner: "alice", vol: "nope", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := r.Allowed(tc.owner, tc.vol); got != tc.want {
				t.Errorf("Allowed(%q, %q) = %v，期望 %v", tc.owner, tc.vol, got, tc.want)
			}
		})
	}

	empty := NewRouter(RouterDeps{})
	if empty.Allowed("alice", "") || empty.Allowed("alice", "disk2") {
		t.Errorf("未装配 Router 应 fail-closed 全拒")
	}
}
