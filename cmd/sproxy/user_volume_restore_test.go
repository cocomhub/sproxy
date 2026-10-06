// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// user_volume_restore_test.go 验证用户卷重启恢复（U4）：ScanRestore → NewBackend →
// Set.AddExternalVolume（单卷失败跳过 + 告警，其余恢复）。

import (
	"context"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

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

type fakeUVRestoreBackend struct{}

func (f *fakeUVRestoreBackend) FS() syncpkg.FS { return nil }
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

// TestRestoreUserVolumes_NestedRebuildsDelegatePool（C1，2026-10-06）：嵌套封装卷重启后
// 重建配额委托——此前仅建卷写内存，重启后 Pool(wrapper)=nil → 配额上限静默失效。本测试
// 钉住 restore 路径重建 DelegatePool（容量=UserVolume.Capacity）+ 父链聚合。
func TestRestoreUserVolumes_NestedRebuildsDelegatePool(t *testing.T) {
	registerUVRestoreBackend()

	root := t.TempDir()
	store := server.NewUserVolumeStore(root)
	if err := store.Create("alice", server.UserVolume{
		Name: "wrap", Type: uvRestoreTestType, Capacity: 50,
		Extra: map[string]any{"target": "base/xyz"},
	}); err != nil {
		t.Fatalf("store.Create(wrap): %v", err)
	}
	// 非嵌套用户卷（容量计数器包装路径不回归）。
	if err := store.Create("alice", server.UserVolume{Name: "plain", Type: uvRestoreTestType, Capacity: 30}); err != nil {
		t.Fatalf("store.Create(plain): %v", err)
	}

	basePool := quota.NewPool(1000)
	set := registry.NewSet(
		[]volume.Volume{{Name: "base"}},
		nil,
		map[string]registry.ExternalBackend{},
		map[string]*quota.Pool{"base": basePool},
		"base",
	)
	if err := restoreUserVolumes(set, store, discardLoggerMain()); err != nil {
		t.Fatalf("restoreUserVolumes: %v", err)
	}
	// C1：嵌套封装卷重启后委托池已重建（Pool(wrap) = 底层池子 Scope，MaxBytes=50）。
	p := set.Pool("wrap")
	if p == nil {
		t.Fatal("wrap 委托池未重建（Pool(wrap) = nil）——配额上限重启后失效")
	}
	if p.MaxBytes() != 50 {
		t.Fatalf("wrap 委托池 MaxBytes=%d want 50", p.MaxBytes())
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
		t.Fatalf("base 底层池 Usage=%d want 10（委托父链聚合）", basePool.Usage())
	}
	// 非嵌套卷不挂委托（Pool(plain) 回落 pools——本 set 无该卷池 → nil；不被误挂子 Scope）。
	if set.Pool("plain") != nil {
		t.Fatalf("plain 不应有委托池（非嵌套 target 不 DelegatePool）")
	}
}
