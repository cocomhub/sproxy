// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_test.go 验证多卷装配（任务 3）：assembleVolumes 逐卷打开根 + 每卷容量池 + ACL 解析、
// resolveDefaultVolumeRoot 的 F1 缺省卷根裁决（storage_root 仅配 volumes 占位不漂移）、以及
// reconcile 双目标框架（owner 全局 Scope 跨卷聚合 + 每卷容量池收敛）。单卷零回归由既有
// integration 全量 + 本文件单卷退化断言共同证明。

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestResolveDefaultVolumeRoot_F1Placeholder 锁定 F1 合入门禁：YAML 只配 storage_root（不写
// volumes）→ Volumes[0].Root 停在占位 defaultStorageRoot——装配必须用 cfg.StorageRoot 作默认卷根，
// 否则存储根静默漂移到 ./storage（单卷零回归红线）。
func TestResolveDefaultVolumeRoot_F1Placeholder(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = "/data"
	// Default() 预合成单默认卷（占位 root=defaultStorageRoot），模拟「未配 volumes」形态。
	if got := resolveDefaultVolumeRoot(cfg); got != "/data" {
		t.Fatalf("resolveDefaultVolumeRoot(占位形态)=%q, want %q（storage_root 裁决）", got, "/data")
	}
}

// TestResolveDefaultVolumeRoot_ExplicitRoot 锁定 F1 反例：首卷显式非占位 root 用其本身。
func TestResolveDefaultVolumeRoot_ExplicitRoot(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = "/data"
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: "/mnt/x"}}
	if got := resolveDefaultVolumeRoot(cfg); got != "/mnt/x" {
		t.Fatalf("resolveDefaultVolumeRoot(显式 /mnt/x)=%q, want /mnt/x", got)
	}
}

// TestResolveDefaultVolumeRoot_EmptyVolumesDefensive 空列表防御：直接回落 StorageRoot。
func TestResolveDefaultVolumeRoot_EmptyVolumesDefensive(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = "/data"
	cfg.Volumes = nil
	if got := resolveDefaultVolumeRoot(cfg); got != "/data" {
		t.Fatalf("resolveDefaultVolumeRoot(空 volumes)=%q, want /data", got)
	}
}

// TestAssembleVolumes_SingleVolumeDegrades 单卷退化：未显式配 volumes（Default + storage_root
// 覆写）→ assembleVolumes 产出 1 卷 name=default，默认卷根 = cfg.StorageRoot（F1），LAYOUT_VERSION
// 就位、池 MaxBytes=0（vol_capacity 缺省不限），ACL 缺省开放。
func TestAssembleVolumes_SingleVolumeDegrades(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root // Volumes[0].Root 保持占位 defaultStorageRoot
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	if len(vs.All()) != 1 || vs.DefaultName != "default" {
		t.Fatalf("单卷退化应有 1 卷 default, got len=%d defaultName=%q", len(vs.All()), vs.DefaultName)
	}
	def := vs.Default()
	if def.Name != "default" || def.RootDir != root {
		t.Fatalf("默认卷不符: %+v（RootDir=%q want %q）", def, def.RootDir, root)
	}
	if vs.DefaultRoot() == nil {
		t.Fatal("DefaultRoot()=nil")
	}
	if vs.Pool("default") == nil || vs.Pool("default").MaxBytes() != 0 {
		t.Fatalf("默认卷池 MaxBytes=%v want 0（vol_capacity 缺省不限）", vs.Pool("default").MaxBytes())
	}
	// LAYOUT_VERSION 就位在 cfg.StorageRoot（而非 ./storage）。
	if _, err := os.Stat(filepath.Join(root, "LAYOUT_VERSION")); err != nil {
		t.Fatalf("storage_root 下 LAYOUT_VERSION 未就位: %v", err)
	}
	if !def.Authorize("alice") {
		t.Fatal("缺省 ACL（未配）= 默认开放，alice 应可用默认卷")
	}
}

// TestAssembleVolumes_F1UsesStorageRootWhenPlaceholder 回归测试（F1 门禁）：storage_root:/data
// 无 volumes → 装配出的默认卷根 = /data 目录（不漂移到 ./storage，也不创建 ./storage）。
func TestAssembleVolumes_F1UsesStorageRootWhenPlaceholder(t *testing.T) {
	storageRoot := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = storageRoot
	if cfg.Volumes[0].Root != defaultStorageRoot {
		t.Fatalf("前置：Default() 首卷 root 应为占位 %q, got %q", defaultStorageRoot, cfg.Volumes[0].Root)
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	if def := vs.Default(); def.RootDir != storageRoot {
		t.Fatalf("默认卷 RootDir=%q, want storage_root=%q（F1 裁决防漂移）", def.RootDir, storageRoot)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "LAYOUT_VERSION")); err != nil {
		t.Fatalf("storage_root 下 LAYOUT_VERSION 未就位: %v", err)
	}
	// 占位形态下不得创建 ./storage（装配根 = cfg.StorageRoot）。
	if _, err := os.Stat(defaultStorageRoot); err == nil {
		t.Fatalf("占位形态装配不应创建 %q 目录（默认卷根应为 cfg.StorageRoot）", defaultStorageRoot)
	}
}

// TestAssembleVolumes_MultiVolumeRoots 多卷根装配：两卷各 OpenRoot（LAYOUT_VERSION 就位）、
// 各建卷容量池（disk2 MaxBytes=100）、anonymous 预创建在 main（默认卷）根。
func TestAssembleVolumes_MultiVolumeRoots(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0]},
		{Name: "disk2", Root: dirs[1], VolCapacity: 100},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	if len(vs.All()) != 2 || vs.DefaultName != "main" {
		t.Fatalf("多卷应有 2 卷且默认 main, got len=%d defaultName=%q", len(vs.All()), vs.DefaultName)
	}
	if vs.Root("main") == nil || vs.Root("disk2") == nil {
		t.Fatal("roots 应含 main/disk2 两卷根")
	}
	if got := vs.Pool("disk2").MaxBytes(); got != 100 {
		t.Fatalf("disk2 卷池 MaxBytes()=%d want 100", got)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "LAYOUT_VERSION")); err != nil {
			t.Fatalf("%s 下 LAYOUT_VERSION 未就位: %v", dir, err)
		}
	}
	if def := vs.Default(); def.Name != "main" || def.RootDir != dirs[0] {
		t.Fatalf("默认卷应为 main@dirs[0], got %+v", def)
	}
	if byName, ok := vs.ByName("disk2"); !ok || byName.Capacity != 100 {
		t.Fatalf("ByName(disk2)=%+v ok=%v want Capacity=100", byName, ok)
	}
}

// TestAssembleVolumes_MultiVolumeWithACL 卷 ACL 解析：allow 白名单 / deny 黑名单经 assemble 后
// Authorize 判定正确；未配 ACL 默认开放。
func TestAssembleVolumes_MultiVolumeWithACL(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0]},
		{Name: "priv", Root: dirs[1], ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"alice"}}},
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	priv, _ := vs.ByName("priv")
	if !priv.Authorize("alice") || priv.Authorize("bob") {
		t.Fatal("allow 白名单：alice 应放行、bob 应拒")
	}
	if !vs.Default().Authorize("bob") {
		t.Fatal("main 未配 ACL → 默认开放")
	}
}

// TestAssembleVolumes_Close 关闭全部卷根（幂等安全：重复 Close 不 panic）。
func TestAssembleVolumes_Close(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "disk2", Root: t.TempDir()},
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

// buildVolSetHandlers 构造带 volSet/globalRoot/globalPool 的 Handlers，供 reconcile 双目标测试
// 使用（等价 RegisterRoutes 装配产物，但不走 HTTP）。
func buildVolSetHandlers(t *testing.T, cfg *Config) *Handlers {
	t.Helper()
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	h := &Handlers{
		cfgPtr:         &cfgPtr,
		logger:         testLogger(),
		auditLogger:    testLogger(),
		uploadingStop:  make(chan struct{}),
		globalRoot:     vs.DefaultRoot(),
		globalPool:     quota.NewPool(cfg.MaxStorageBytes),
		volSet:         vs,
		tenantRoots:    make(map[string]*storage.Tenant),
		checksumStores: make(map[string]*checksum.ChecksumStore),
		uploadStores:   make(map[string]*UploadStore),
		quotaScopes:    make(map[string]*quota.Scope),
		quotaBuckets:   make(map[string]map[string]*quota.Scope),
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestReconcileVolumes_MultiVolumeDoubleTarget 验证 reconcile 双目标框架：
//  1. owner 全局 Scope 校准到跨卷合计（alice 两卷 user+cloud = 90）；
//  2. 每卷容量池各自收敛到该卷磁盘占用（main 60 / disk2 30）。
func TestReconcileVolumes_MultiVolumeDoubleTarget(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.MaxStorageBytes = 10000
	cfg.OwnerQuotas = map[string]int64{"alice": 300}
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0]},
		{Name: "disk2", Root: dirs[1], VolCapacity: 100},
	}
	h := buildVolSetHandlers(t, cfg)

	volumeBuckets := map[string]map[string]map[string]int64{
		"main":  {"alice": {"user": 40, "cloud": 20}},
		"disk2": {"alice": {"user": 30}},
	}
	h.reconcileVolumes(volumeBuckets)

	// owner 全局 Scope 校准到跨卷合计（90）。
	if got := h.quotaFor("alice").Usage(); got != 90 {
		t.Fatalf("alice 全局 Scope Usage()=%d want 90（跨卷合计）", got)
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 70 {
		t.Fatalf("alice user 桶 Usage()=%d want 70（main 40 + disk2 30）", got)
	}
	// 每卷容量池收敛。
	if got := h.volSet.Pool("main").Usage(); got != 60 {
		t.Fatalf("main 卷池 Usage()=%d want 60", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 30 {
		t.Fatalf("disk2 卷池 Usage()=%d want 30", got)
	}
	// 卷容量上限不因校准改变（MaxBytes 保持配置值）。
	if got := h.volSet.Pool("disk2").MaxBytes(); got != 100 {
		t.Fatalf("disk2 卷池 MaxBytes()=%d want 100", got)
	}
}

// TestReconcileVolumePool_SingleVolume 单卷 reconcile（RegisterRoutes 装配形态）：owner Scope +
// 默认卷池双校准，卷池在途预留时跳过（双计保护）。
func TestReconcileVolumePool_SingleVolume(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.MaxStorageBytes = 10000
	h := buildVolSetHandlers(t, cfg)

	tenantBuckets := map[string]map[string]int64{
		"alice": {"user": 50},
	}
	h.reconcileVolumePool("default", tenantBuckets)

	if got := h.quotaFor("alice").Usage(); got != 50 {
		t.Fatalf("alice 全局 Scope Usage()=%d want 50", got)
	}
	if got := h.volSet.Pool("default").Usage(); got != 50 {
		t.Fatalf("default 卷池 Usage()=%d want 50", got)
	}

	// 在途预留 → 校准跳过（双计保护）：user 桶预留 20 未 Commit 时再次 reconcile，
	// user 桶 committed 不被「磁盘 50 + reserved 20」双计污染（reconcileQuotaScopes skip 语义）。
	rr, err := h.quotaBucketFor("alice", "user").TryReserve(20)
	if err != nil {
		t.Fatal(err)
	}
	tenantBuckets2 := map[string]map[string]int64{
		"alice": {"user": 50},
	}
	h.reconcileVolumePool("default", tenantBuckets2)
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 50 {
		t.Fatalf("在途预留后 user 桶 Usage()=%d want 50（skip 双计保护）", got)
	}
	rr.Release()
}

// TestReconcileVolumePool_NestedDirKeyNoDoubleCount 回归 Important-1：StorageManager 真实扫描会
// 对嵌套 user 文件同时产出功能桶键（user）与子目录键（user/videos），adjustVolumePool 只按功能桶
// 顶层键求和——嵌套文件不双计，卷池 Usage == 物理字节。
func TestReconcileVolumePool_NestedDirKeyNoDoubleCount(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.MaxStorageBytes = 10000
	h := buildVolSetHandlers(t, cfg)

	// 真实扫描形态：alice/user/a.txt(100) + alice/user/videos/b.mkv(200) + cloud/t1/c.bin(50)
	// → tenantBuckets={alice:{user:300, user/videos:200, cloud:50}}（user 已含嵌套文件字节）。
	tenantBuckets := map[string]map[string]int64{
		"alice": {"user": 300, "user/videos": 200, "cloud": 50},
	}
	h.reconcileVolumePool("default", tenantBuckets)

	// 卷池 = 物理字节 350（300+50），而非 550（300+200+50 嵌套双计）。
	if got := h.volSet.Pool("default").Usage(); got != 350 {
		t.Fatalf("default 卷池 Usage()=%d want 350（嵌套 user/videos 只计一次，不双计）", got)
	}
	// owner 全局 Scope 同样收敛到物理字节（reconcileQuotaScopes 先深后浅已消重）。
	if got := h.quotaFor("alice").Usage(); got != 350 {
		t.Fatalf("alice 全局 Scope Usage()=%d want 350", got)
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 300 {
		t.Fatalf("alice user 桶 Usage()=%d want 300", got)
	}
}

// TestReconcileVolumes_VolumeTypeSmoke 确保 registry.Set 与 pkg/volume 域类型打通（装配产物可直接
// 作为 volume.AllowedVolumes / OrderCandidates 输入——T4 路由的输入形状）。
func TestReconcileVolumes_VolumeTypeSmoke(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "priv", Root: t.TempDir(), ACL: &VolumeACLConfig{Mode: VolumeACLAllow, Owners: []string{"alice"}}},
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	allowed := volume.AllowedVolumes(vs.All(), "bob")
	if len(allowed) != 1 || allowed[0].Name != "main" {
		t.Fatalf("bob 应仅见 main（priv allow 仅 alice）, got %+v", allowed)
	}
	allowedAlice := volume.AllowedVolumes(vs.All(), "alice")
	if len(allowedAlice) != 2 {
		t.Fatalf("alice 应见两卷, got %+v", allowedAlice)
	}
	// prefer-default 首卷 main 恒前。
	ordered := volume.OrderCandidates(allowedAlice, volume.ModePreferDefault, nil)
	if ordered[0].Name != "main" {
		t.Fatalf("prefer-default 首卷 main 应恒前, got %+v", ordered)
	}
}
