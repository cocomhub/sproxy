// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// user_volume_restore_test.go 验证用户卷重启恢复（U4）：ScanRestore →（嵌套封装卷经装配层
// 翻译走 enclosure 底座；其余类型走 registry.NewBackend）→ 注册进卷集（单卷失败跳过 + 告警）。

import (
	"context"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/enclosure"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// registerUVRestoreWrapper 为测试类型注册**同名封装处理层**：使「该类型 + 嵌套 target」的用户
// 卷声明在恢复时被翻译出封装声明词汇（嵌套装配的先决条件；卷 Type 保持 wrapper 类型）。
func registerUVRestoreWrapper() {
	uvRestoreWrapperOnce.Do(func() {
		enclosure.RegisterWrapper(uvRestorePassthru{})
	})
}

var uvRestoreWrapperOnce sync.Once

// uvRestorePassthru 是「原样透传源视图」的封装处理层替身（只做区域映射，不改字节）。
type uvRestorePassthru struct{}

func (uvRestorePassthru) Name() string { return uvRestoreTestType }

func (uvRestorePassthru) Build(_ context.Context, _ volume.Volume, src syncpkg.FS, _ map[string]any) (registry.ExternalBackend, error) {
	return &fakeUVRestoreBackend{fs: src}, nil
}

// uvRestoreTestType 是 cmd 侧测试后端类型（唯一，避免与生产 baidupcs 冲突）。
const uvRestoreTestType = "user-vol-restore-test"

var uvRestoreBackendOnce sync.Once

// registerUVRestoreBackend 注册测试后端（sync.Once 防重复 panic）。
func registerUVRestoreBackend() {
	uvRestoreBackendOnce.Do(func() {
		registry.RegisterBackend(uvRestoreTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return &fakeUVRestoreBackend{}, nil
		})
	})
}

type fakeUVRestoreBackend struct{ fs syncpkg.FS }

func (f *fakeUVRestoreBackend) FS() syncpkg.FS { return f.fs }
func (f *fakeUVRestoreBackend) Close() error   { return nil }

// fakeUVRestoreFS 占位 FS 类型（不实例化；ExternalBackend.FS 返回 nil 已满足接口）。

// badUVType 是构造必败的后端类型（恢复单卷失败跳过验证）。
const badUVType = "user-vol-restore-bad"

var badUVBackendOnce sync.Once

func registerBadUVBackend() {
	badUVBackendOnce.Do(func() {
		registry.RegisterBackend(badUVType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			return nil, context.DeadlineExceeded
		})
	})
}

// TestRestoreUserVolumes_PartialFail 单卷恢复失败跳过 + 其余恢复（不整体失败）。
func TestRestoreUserVolumes_PartialFail(t *testing.T) {
	t.Parallel()
	registerUVRestoreBackend()
	registerBadUVBackend()

	root := t.TempDir()
	store := server.NewUserVolumeStore(root)
	if err := store.Create("alice", server.UserVolume{Name: "good-disk", Type: uvRestoreTestType}); err != nil {
		t.Fatalf("store.Create(good): %v", err)
	}
	if err := store.Create("alice", server.UserVolume{Name: "bad-disk", Type: badUVType}); err != nil {
		t.Fatalf("store.Create(bad): %v", err)
	}

	set := registry.NewSet([]volume.Volume{}, nil, map[string]registry.ExternalBackend{}, nil, "")
	if err := restoreUserVolumes(set, store, discardLoggerMain()); err != nil {
		t.Fatalf("restoreUserVolumes: %v", err)
	}
	if set.External("good-disk") == nil {
		t.Fatal("good-disk 应恢复（坏卷失败不应阻塞）")
	}
	if set.External("bad-disk") != nil {
		t.Fatal("bad-disk 构造失败应跳过（不进 Set.external）")
	}
}

// TestRestoreUserVolumes 重启恢复：ScanRestore 的卷经 NewBackend 构造后 AddExternalVolume。
func TestRestoreUserVolumes(t *testing.T) {
	t.Parallel()
	registerUVRestoreBackend()

	root := t.TempDir()
	store := server.NewUserVolumeStore(root)
	if err := store.Create("alice", server.UserVolume{Name: "disk-1", Type: uvRestoreTestType, Capacity: 100}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if err := store.Create("bob", server.UserVolume{Name: "disk-2", Type: uvRestoreTestType, Capacity: 200}); err != nil {
		t.Fatalf("store.Create: %v", err)
	}

	set := registry.NewSet([]volume.Volume{}, nil, map[string]registry.ExternalBackend{}, nil, "")
	if err := restoreUserVolumes(set, store, discardLoggerMain()); err != nil {
		t.Fatalf("restoreUserVolumes: %v", err)
	}
	// 两卷都应恢复进 Set（External 可查）。
	if set.External("disk-1") == nil {
		t.Fatal("disk-1 应恢复到 Set.external")
	}
	if set.External("disk-2") == nil {
		t.Fatal("disk-2 应恢复到 Set.external")
	}
	// 卷元数据含 Type/Capacity（AddExternalVolume 的 Volume 完整）；Owner 由 store 闭包校验
	// （Set 不存 owner——SetUserVolumeOwner 查 store.Get(owner,name)）。
	if v, ok := set.ByName("disk-1"); !ok || v.Type != uvRestoreTestType || v.Capacity != 100 {
		t.Fatalf("disk-1 元数据 = %+v, want type=%s cap=100", v, uvRestoreTestType)
	}
}

// TestRestoreUserVolumes_NestedInheritsSourcePool（阶段 2 迁移）：封装卷（用户面
// `target: "<源卷>/<子目录>"`）重启恢复后，其容量池必须是**继承源卷池的子池**
// （`quota.NewPoolWithParent(源卷池, max_bytes)`）——取代过渡层的 `DelegatePool` 子 Scope。
// 语义等价物：提交进自身池即沿父链聚合到源卷池（整体不超源卷配额），且 `max_bytes`
// （= UserVolume.Capacity）作为自身上限生效。
//
// 变异：去掉 restore 路径的封装装配（或翻译）→ Pool(wrap)==nil → 红。
func TestRestoreUserVolumes_NestedInheritsSourcePool(t *testing.T) {
	t.Parallel()
	registerUVRestoreBackend()
	registerUVRestoreWrapper()

	root := t.TempDir()
	rt, oerr := storage.OpenRoot(root)
	if oerr != nil {
		t.Fatalf("OpenRoot: %v", oerr)
	}
	store := server.NewUserVolumeStore(t.TempDir())
	if err := store.Create("alice", server.UserVolume{
		Name: "wrap", Type: uvRestoreTestType, Capacity: 50,
		Extra: map[string]any{"target": "base/xyz"},
	}); err != nil {
		t.Fatalf("store.Create(wrap): %v", err)
	}
	// 非嵌套用户卷（不受影响：按普通外部卷 + CapacityFS 计数器恢复）。
	if err := store.Create("alice", server.UserVolume{Name: "plain", Type: uvRestoreTestType, Capacity: 30}); err != nil {
		t.Fatalf("store.Create(plain): %v", err)
	}

	basePool := quota.NewPool(1000)
	set := registry.NewSet(
		[]volume.Volume{{Name: "base", Type: volume.TypeLocal, RootDir: root}},
		map[string]*storage.Root{"base": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"base": basePool},
		"base",
	)
	t.Cleanup(func() { _ = set.Close() })
	if err := restoreUserVolumes(set, store, discardLoggerMain()); err != nil {
		t.Fatalf("restoreUserVolumes: %v", err)
	}
	// 继承池已建（MaxBytes = UserVolume.Capacity；父 = 源卷池）。
	p := set.Pool("wrap")
	if p == nil {
		t.Fatal("wrap 继承池未重建（Pool(wrap) = nil）——配额上限重启后失效")
	}
	if p.MaxBytes() != 50 {
		t.Fatalf("wrap 继承池 MaxBytes=%d want 50", p.MaxBytes())
	}
	if p.Parent() != basePool {
		t.Fatal("wrap 池应挂到源卷池之下（配额继承）")
	}
	// 父链聚合：子池入账自动传播到底层池。
	res, rerr := p.TryReserve(10)
	if rerr != nil {
		t.Fatalf("TryReserve: %v", rerr)
	}
	res.Commit(10)
	if p.Usage() != 10 {
		t.Fatalf("wrap 子池 Usage=%d want 10", p.Usage())
	}
	if basePool.Usage() != 10 {
		t.Fatalf("base 底层池 Usage=%d want 10（继承父链聚合）", basePool.Usage())
	}
	// 非嵌套卷不建继承池（回落 pools——本 set 无该卷池 → nil）。
	if set.Pool("plain") != nil {
		t.Fatalf("plain 不应有继承池（非嵌套 target 不继承）")
	}
	// 源卷封印生效（占用区间只读是封装卷的核心不变量）。
	if rt.SealAt("xyz/x") == nil {
		t.Fatalf("恢复封装卷后源卷 base 的 xyz 区间应被封印, rules=%+v", rt.Seals())
	}
}

// TestRestoreUserVolumes_ChainedPoolOrder（阶段 2 迁移）：链式嵌套恢复按依赖分轮——
// 底层卷名序在封装卷后（base "zzz" 名序在 wrapper "aaa" 之后，ScanRestore 按 (owner,name)
// 升序返回 aaa 先于 zzz）时，封装卷的装配不再因源卷未就绪而早失败。断言两层继承池均重建且
// 外层池挂在**内层池**之下（父链一路聚合到基底 base 卷池）。
func TestRestoreUserVolumes_ChainedPoolOrder(t *testing.T) {
	t.Parallel()
	registerUVRestoreBackend()
	registerUVRestoreWrapper()

	baseRoot := t.TempDir()
	rt, oerr := storage.OpenRoot(baseRoot)
	if oerr != nil {
		t.Fatalf("OpenRoot: %v", oerr)
	}
	store := server.NewUserVolumeStore(t.TempDir())
	// 名称序：aaa < zzz（ScanRestore 升序返回 aaa 先），zzz 是 aaa 的底层。
	if err := store.Create("alice", server.UserVolume{
		Name: "aaa", Type: uvRestoreTestType, Capacity: 20,
		Extra: map[string]any{"target": "zzz/xyz"},
	}); err != nil {
		t.Fatalf("store.Create(aaa): %v", err)
	}
	if err := store.Create("alice", server.UserVolume{
		Name: "zzz", Type: uvRestoreTestType, Capacity: 30,
		Extra: map[string]any{"target": "base/x1"},
	}); err != nil {
		t.Fatalf("store.Create(zzz): %v", err)
	}

	basePool := quota.NewPool(1000)
	set := registry.NewSet(
		[]volume.Volume{{Name: "base", Type: volume.TypeLocal, RootDir: baseRoot}},
		map[string]*storage.Root{"base": rt},
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"base": basePool},
		"base",
	)
	t.Cleanup(func() { _ = set.Close() })
	if err := restoreUserVolumes(set, store, discardLoggerMain()); err != nil {
		t.Fatalf("restoreUserVolumes: %v", err)
	}
	// zzz 继承池已重建（源 base）。
	pz := set.Pool("zzz")
	if pz == nil || pz.MaxBytes() != 30 {
		t.Fatalf("zzz 继承池未重建（base 名序在后不应影响源卷）: %+v", pz)
	}
	// aaa 继承池已重建（挂 zzz 池之下）——链式依赖未因排序早失败。
	pa := set.Pool("aaa")
	if pa == nil || pa.MaxBytes() != 20 {
		t.Fatalf("aaa 继承池未重建（依赖 zzz，先扫到的 aaa 应被分轮推迟）: %+v", pa)
	}
	// 继承挂在**有效源卷**（Enclosure.EffectiveTarget 沿链回溯到真正承载字节的基底卷）之下：
	// 链式嵌套的两个池都直挂 base 池（与装配期 inheritPools 同口径——避免同一字节在中间层
	// 重复计一次）。
	if pa.Parent() != basePool {
		t.Fatal("aaa 池应挂在有效源卷 base 池之下（EffectiveTarget 回溯）")
	}
	// 父链聚合：aaa 入账传导到底层 base 池。
	res, rerr := pa.TryReserve(7)
	if rerr != nil {
		t.Fatalf("TryReserve: %v", rerr)
	}
	res.Commit(7)
	if basePool.Usage() != 7 {
		t.Fatalf("父链聚合失败: base=%d want 7", basePool.Usage())
	}
	if pz.Usage() != 0 {
		t.Fatalf("zzz 自身无占用（aaa 的字节不经 zzz 池记账）: %d", pz.Usage())
	}
}
