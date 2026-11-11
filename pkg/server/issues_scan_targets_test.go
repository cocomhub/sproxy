// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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

func (f *inspectorFS) Trace(ctx context.Context, rel string, mode volume.InspectMode) (*volume.PathTrace, error) {
	return f.insp.Trace(ctx, rel, mode)
}
func (f *inspectorFS) Explain(ctx context.Context, rel string, mode volume.InspectMode) (*volume.RealExplanation, error) {
	return f.insp.Explain(ctx, rel, mode)
}
func (f *inspectorFS) TraceTree(ctx context.Context, rel string, depth int, mode volume.InspectMode) (*volume.PathTrace, error) {
	return f.insp.TraceTree(ctx, rel, depth, mode)
}

// TestScanTargetsFromSet_OnlyInspectable 锁定：只挑「后端 FS 实现 StorageInspector」的卷；
// 不支持审查的卷静默跳过（可选能力，属正常），nil 卷集返回空。
func TestScanTargetsFromSet_OnlyInspectable(t *testing.T) {
	t.Parallel()
	insp := &fakeInspector{trace: &volume.PathTrace{Rel: "", Kind: "dir"}}
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
