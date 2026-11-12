// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
)

// fakeLister 是卷集最小接口的测试替身。
type fakeLister struct {
	vols []volume.Volume
	vs   map[string]syncpkg.FS
}

func (f *fakeLister) All() []volume.Volume { return f.vols }
func (f *fakeLister) VolumeView(name string, _ registry.ViewUse) (syncpkg.FS, bool) {
	fs, ok := f.vs[name]
	return fs, ok
}

// inspectorFS 把「审查能力」包进一个 sync.FS（嵌入接口 = 只实现需要的方法，
// 避免写 7 个空方法）。这里只为让 AssertStorageInspector 的类型断言成立。
type inspectorFS struct {
	syncpkg.FS
	insp *fakeInspector
}

func (f *inspectorFS) Trace(ctx context.Context, rel string, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	return f.insp.Trace(ctx, rel, mode)
}
func (f *inspectorFS) Explain(ctx context.Context, rel string, mode syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
	return f.insp.Explain(ctx, rel, mode)
}
func (f *inspectorFS) TraceTree(ctx context.Context, rel string, depth int, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	return f.insp.TraceTree(ctx, rel, depth, mode)
}

// TestScanTargetsFromSet_OnlyInspectable 锁定：只挑「后端 FS 实现 StorageInspector」的卷；
// 不支持审查的卷静默跳过（可选能力，属正常），nil 卷集返回空。
func TestScanTargetsFromSet_OnlyInspectable(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{trace: &syncpkg.PathTrace{Rel: "", Kind: "dir"}}
	l := &fakeLister{
		vols: []volume.Volume{{Name: "baidu"}, {Name: "local"}},
		vs: map[string]syncpkg.FS{
			"baidu": &inspectorFS{insp: insp},
			// local 的视图不支持审查（nil ⇒ 取不到/断言失败 ⇒ 跳过）
			"local": nil,
		},
	}
	got := ScanTargetsFromSet(l)
	if len(got) != 1 || got[0].Volume != "baidu" {
		t.Fatalf("应只挑出可审查的 baidu 卷，got %+v", got)
	}
	if ScanTargetsFromSet(nil) != nil {
		t.Fatal("nil 卷集应返回 nil")
	}
}

// TestScanTargetsFromSet_SkipsMissingBackend 锁定：没有任何后端的卷被跳过（不 panic）。
func TestScanTargetsFromSet_SkipsMissingBackend(t *testing.T) {
	t.Parallel()
	l := &fakeLister{vols: []volume.Volume{{Name: "ghost"}}, vs: map[string]syncpkg.FS{}}
	if got := ScanTargetsFromSet(l); len(got) != 0 {
		t.Fatalf("无后端应跳过，got %+v", got)
	}
}

// cleanerFS 把「目录 meta 清理能力」包进 sync.FS（嵌入接口 = 只实现需要的方法）。
type cleanerFS struct {
	syncpkg.FS
	removed int
	got     []string
}

func (f *cleanerFS) CleanupStaleDirMeta(_ context.Context, container string) (int, error) {
	f.got = append(f.got, container)
	return f.removed, nil
}

// TestCleanupTargetResolver_DecoratedViewForwardsCapability 锁定：解析器经统一卷视图取 FS 后
// **直接断言装饰后的 FS**（不再 `Innermost` 解包），且能力在装饰链上**真的转发**到最内层实现。
//
// 为什么能不再解包（2026-11-12 收口）：`syncpkg.DirMetaCleaner` 已下沉 `pkg/sync`，R21 门禁
// 自动枚举它并要求 11 个装饰器全量转发 ⇒ 装饰链不再吞掉该能力。
//
// 变异（两类都必须红）：① 去掉任一装饰器的转发方法/断言行 ⇒ R21 门禁红；
// ② 解析器退回直连后端视图（或解包取错层）⇒ 本用例的"装饰链转发命中"断言红。
func TestCleanupTargetResolver_DecoratedViewForwardsCapability(t *testing.T) {
	t.Parallel()
	impl := &cleanerFS{removed: 2}
	// 生产同形的装饰链：trusted.Guard → trusted.Wrap → PrefixFS（中间层）→ 领域实现。
	view := trusted.Guard(trusted.Wrap(syncpkg.NewPrefixFS(impl, "pre/"), trusted.Options{}))
	l := &fakeLister{
		vols: []volume.Volume{{Name: "sd"}, {Name: "plain"}, {Name: "missing"}},
		vs: map[string]syncpkg.FS{
			"sd": view,
			// "plain" 视图为 nil（取不到）；"missing" 未装配
			"plain": nil,
		},
	}
	resolve := CleanupTargetResolver(l)

	got := resolve("sd")
	// 锁死「不再解包」契约：返回的必须是**装饰后的视图本身**，而非最内层实现。
	if got != any(view) {
		t.Fatalf("解析器应直接返回装饰后的视图（不再解包），got %T", got)
	}
	cleaner, ok := syncpkg.AssertDirMetaCleaner(got)
	if !ok {
		t.Fatalf("装饰后的 FS 应实现 DirMetaCleaner（装饰器必须全量转发），got %T", got)
	}
	if n, err := cleaner.CleanupStaleDirMeta(t.Context(), "OypE/abc/ContainerX"); err != nil || n != 2 {
		t.Fatalf("CleanupStaleDirMeta = (%d, %v)，want (2, nil)", n, err)
	}
	if len(impl.got) != 1 || impl.got[0] != "OypE/abc/ContainerX" {
		t.Fatalf("容器路径应经装饰链原样转发到领域实现，got %v", impl.got)
	}
	if resolve("plain") != nil {
		t.Fatalf("视图为 nil 的卷应返回 nil，got %T", resolve("plain"))
	}
	if resolve("missing") != nil {
		t.Fatal("未装配的卷应返回 nil")
	}
	if CleanupTargetResolver(nil)("x") != nil {
		t.Fatal("nil 卷集应返回 nil")
	}
}
