// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// baidupcs_sync_test.go 钉住 P4 **baidupcs 载体装配**的行为：
//   - 未启用 → 不注入工厂（kind=baidupcs 远端 fail-closed）；
//   - 启用 + fake 工厂 → 工厂注入，按 remote.Volume 查返回 StorageFS，push/pull 可跑；
//   - 工厂返回错误 → 不注入（fail-closed，绝不回落 direct）；
//   - quota 适配器（ownerQuotaTracker，P5 per-owner）：ReserveUsage = TryReserve+Commit（计数器入账）、
//     ReleaseUsage = ReleaseUsage（扣减），对齐 P2 Quota 语义。

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// fakeBaidupcsStorage 是内存 StorageAPI（测试用，实现 6 方法）。
type fakeBaidupcsStorage struct {
	files map[string][]byte
}

func newFakeBaidupcsStorage() *fakeBaidupcsStorage {
	return &fakeBaidupcsStorage{files: map[string][]byte{}}
}

func (f *fakeBaidupcsStorage) Put(ctx context.Context, key string, r io.Reader) (*baidupcs.ObjectMeta, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.files[key] = data
	return &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeBaidupcsStorage) Get(ctx context.Context, key string) (io.ReadCloser, *baidupcs.ObjectMeta, error) {
	data, ok := f.files[key]
	if !ok {
		return nil, nil, baidupcs.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeBaidupcsStorage) Stat(ctx context.Context, key string) (*baidupcs.ObjectMeta, error) {
	data, ok := f.files[key]
	if !ok {
		return nil, baidupcs.ErrNotFound
	}
	return &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeBaidupcsStorage) List(ctx context.Context, prefix string) ([]baidupcs.ObjectMeta, error) {
	return nil, nil
}

func (f *fakeBaidupcsStorage) Delete(ctx context.Context, key string) error {
	delete(f.files, key)
	return nil
}

func (f *fakeBaidupcsStorage) Copy(ctx context.Context, srcKey, dstKey string) (*baidupcs.ObjectMeta, error) {
	data, ok := f.files[srcKey]
	if !ok {
		return nil, baidupcs.ErrNotFound
	}
	f.files[dstKey] = data
	return &baidupcs.ObjectMeta{Key: dstKey, Size: int64(len(data))}, nil
}

var _ baidupcs.StorageAPI = (*fakeBaidupcsStorage)(nil)

// TestSetupBaidupcsFSFactory_NoBaidupcsVolumes 无外部卷的 Set → 工厂注入但查任意卷都报错（fail-closed 在调用点）。
func TestSetupBaidupcsFSFactory_NoBaidupcsVolumes(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	set := registry.NewSet(nil, nil, nil, nil, "")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("应注入工厂（查卷失败在调用点 fail-closed）")
	}
	_, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-x", Kind: syncmgr.RemoteKindBaidupcs, Volume: "any",
	}, "test-owner")
	if err == nil {
		t.Fatal("无外部卷时查任意卷应报错（fail-closed）")
	}
}

// setupBaidupcsTestSet 构造含 baidupcs external backend 的 registry.Set（测试用）。
// 经 registerBaidupcsBackendWithFactory 注入 fake Storage 工厂 → NewBackend 构造 backend → external。
func setupBaidupcsTestSet(t *testing.T, typ string, factory baidupcsStorageFactory, names ...string) *registry.Set {
	t.Helper()
	registerBaidupcsBackendWithFactory(typ, factory)
	external := make(map[string]registry.ExternalBackend, len(names))
	volumes := make([]volume.Volume, 0, len(names))
	for _, name := range names {
		v := volume.Volume{
			Name:    name,
			Type:    typ,
			RootDir: t.TempDir(),
			Extra:   map[string]any{"bduss": "test-bduss"},
		}
		be, err := registry.NewBackend(context.Background(), v)
		if err != nil {
			t.Fatalf("NewBackend(%s): %v", name, err)
		}
		external[name] = be
		volumes = append(volumes, v)
	}
	return registry.NewSet(volumes, nil, external, nil, "")
}

// TestSetupBaidupcsFSFactory_RegistryLookup 装配含 baidupcs 卷（Set.External）→ 注入工厂，
// 按 remote.Volume 查 Set.External 返回 FS。
func TestSetupBaidupcsFSFactory_RegistryLookup(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	st := newFakeBaidupcsStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) { return st, nil }
	set := setupBaidupcsTestSet(t, "baidupcs-t3-lookup", factory, "mydisk")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("装配 baidupcs 卷应注入 BaidupcsFS 工厂")
	}
	fs, closeFn, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	}, "test-owner")
	if err != nil {
		t.Fatalf("工厂按 volume 查: %v", err)
	}
	if closeFn == nil {
		t.Fatal("closeFn 不应为 nil")
	}
	closeFn()
	if fs == nil {
		t.Fatal("工厂应返回非 nil FS")
	}
	// 用 FS 真跑一次 WriteFile（fake 内存网盘）→ 证明装配链路可用（Set.External → StorageFS）。
	if writeErr := fs.WriteFile(context.Background(), "x.txt", bytes.NewReader([]byte("netdisk")), 7, 0); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}
	rc, openErr := fs.OpenRead(context.Background(), "x.txt")
	if openErr != nil {
		t.Fatalf("OpenRead: %v", openErr)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "netdisk" {
		t.Fatalf("内容 = %q, want %q", got, "netdisk")
	}
}

// TestSetupBaidupcsFSFactory_VolumeMissing 工厂注入，但 remote.Volume 未装配（Set.External nil）→ 明确错误。
func TestSetupBaidupcsFSFactory_VolumeMissing(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return newFakeBaidupcsStorage(), nil
	}
	set := setupBaidupcsTestSet(t, "baidupcs-t3-missing", factory, "mydisk")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("应注入工厂")
	}
	_, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "nonexistent",
	}, "test-owner")
	if err == nil {
		t.Fatal("未装配卷应报错（fail-closed）")
	}
}

// TestSetupBaidupcsFSFactory_AllVolumesFailed 全部卷 backend 构造失败（Set.External 空）→ 工厂注入但查卷报错（fail-closed 在调用点）。
func TestSetupBaidupcsFSFactory_AllVolumesFailed(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	// 注册一个构造必失败的 backend（factory 返回错误）→ NewBackend 失败 → external 空。
	typ := "baidupcs-t3-allfail"
	registerBaidupcsBackendWithFactory(typ, func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return nil, baidupcs.ErrInvalidParam
	})
	v := volume.Volume{Name: "bad", Type: typ, RootDir: t.TempDir(), Extra: map[string]any{"bduss": "x"}}
	if _, err := registry.NewBackend(context.Background(), v); err == nil {
		t.Fatal("构造失败的 backend 应报错")
	}
	// external 空 → 工厂注入，但查卷失败（调用点 fail-closed）。
	set := registry.NewSet(nil, nil, nil, nil, "")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("应注入工厂（查卷失败在调用点 fail-closed）")
	}
	_, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-x", Kind: syncmgr.RemoteKindBaidupcs, Volume: "bad",
	}, "test-owner")
	if err == nil {
		t.Fatal("无 external 时查卷应报错（fail-closed）")
	}
}

// TestSetupBaidupcsFSFactory_MultiDisk 多盘装配（Set.External 2 盘）→ 工厂按 remote.Volume 查对
// （disk1 → 盘1 的 FS，卷隔离）。
func TestSetupBaidupcsFSFactory_MultiDisk(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	st1 := newFakeBaidupcsStorage()
	st2 := newFakeBaidupcsStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		switch cfg.BDUSS {
		case "bduss-1":
			return st1, nil
		case "bduss-2":
			return st2, nil
		default:
			return nil, baidupcs.ErrInvalidParam
		}
	}
	typ := "baidupcs-t3-multi"
	registerBaidupcsBackendWithFactory(typ, factory)
	external := make(map[string]registry.ExternalBackend, 2)
	volumes := make([]volume.Volume, 0, 2)
	for _, name := range []string{"disk1", "disk2"} {
		bduss := "bduss-1"
		if name == "disk2" {
			bduss = "bduss-2"
		}
		v := volume.Volume{Name: name, Type: typ, RootDir: t.TempDir(), Extra: map[string]any{"bduss": bduss}}
		be, err := registry.NewBackend(context.Background(), v)
		if err != nil {
			t.Fatalf("NewBackend(%s): %v", name, err)
		}
		external[name] = be
		volumes = append(volumes, v)
	}
	set := registry.NewSet(volumes, nil, external, nil, "")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("多盘装配应注入 BaidupcsFS 工厂")
	}
	// 按 volume 查对：disk1 → 盘1 的 FS（写盘1 → 盘2 不可见）。
	fs1, close1, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk1",
	}, "test-owner")
	if err != nil {
		t.Fatalf("工厂查 disk1: %v", err)
	}
	defer close1()
	if writeErr := fs1.WriteFile(context.Background(), "x.txt", bytes.NewReader([]byte("disk1-data")), 9, 0); writeErr != nil {
		t.Fatalf("disk1 WriteFile: %v", writeErr)
	}
	if _, ok := st1.files["x.txt"]; !ok {
		t.Fatal("disk1 文件应写入盘1")
	}
	if _, ok := st2.files["x.txt"]; ok {
		t.Fatal("disk1 文件不应写入盘2（卷隔离）")
	}
	// disk2 → 盘2 的 FS。
	fs2, close2, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r2", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk2",
	}, "test-owner")
	if err != nil {
		t.Fatalf("工厂查 disk2: %v", err)
	}
	defer close2()
	if fs2 == nil {
		t.Fatal("disk2 工厂应返回非 nil FS")
	}
}

// TestSetupBaidupcsFSFactory_PartialFail 多盘部分失败容忍：盘2 构造失败 → 盘1 仍装配，
// 工厂查盘2 报「卷未装配」（fail-closed）。
func TestSetupBaidupcsFSFactory_PartialFail(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		if cfg.BDUSS == "bduss-2" {
			return nil, baidupcs.ErrInvalidParam
		}
		return newFakeBaidupcsStorage(), nil
	}
	typ := "baidupcs-t3-partial"
	registerBaidupcsBackendWithFactory(typ, factory)
	// 盘1 成功进 external；盘2 构造失败 → 不进 external。
	v1 := volume.Volume{Name: "disk1", Type: typ, RootDir: t.TempDir(), Extra: map[string]any{"bduss": "bduss-1"}}
	be1, err := registry.NewBackend(context.Background(), v1)
	if err != nil {
		t.Fatalf("NewBackend(disk1): %v", err)
	}
	v2 := volume.Volume{Name: "disk2", Type: typ, RootDir: t.TempDir(), Extra: map[string]any{"bduss": "bduss-2"}}
	if _, v2Err := registry.NewBackend(context.Background(), v2); v2Err == nil {
		t.Fatal("盘2 构造应失败")
	}
	set := registry.NewSet([]volume.Volume{v1}, nil, map[string]registry.ExternalBackend{"disk1": be1}, nil, "")
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), nil)
	if exec.BaidupcsFS == nil {
		t.Fatal("部分失败仍应注入工厂（盘1 可用）")
	}
	// 盘1 可用。
	fs1, close1, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk1",
	}, "test-owner")
	if err != nil {
		t.Fatalf("盘1 应可用: %v", err)
	}
	close1()
	if fs1 == nil {
		t.Fatal("盘1 工厂应返回非 nil FS")
	}
	// 盘2 未装配（external 无）→ 明确报错。
	_, _, err = exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r2", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk2",
	}, "test-owner")
	if err == nil {
		t.Fatal("构造失败的盘应报「卷未装配」（fail-closed）")
	}
}

// TestOwnerQuotaTracker 适配器计数器语义（P5 per-owner）：预留入账 committed，释放扣减。
func TestOwnerQuotaTracker(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	scope := pool.Scope("", 0)
	q := &ownerQuotaTracker{scope: scope}
	if err := q.ReserveUsage(40); err != nil {
		t.Fatalf("ReserveUsage(40): %v", err)
	}
	if got := scope.Usage(); got != 40 {
		t.Fatalf("Usage after reserve = %d, want 40", got)
	}
	if err := q.ReserveUsage(50); err != nil {
		t.Fatalf("ReserveUsage(50): %v", err)
	}
	if got := scope.Usage(); got != 90 {
		t.Fatalf("Usage = %d, want 90", got)
	}
	// 超限 → 错误。
	if err := q.ReserveUsage(50); err == nil {
		t.Fatal("超限应报错")
	}
	q.ReleaseUsage(40)
	q.ReleaseUsage(50)
	if got := scope.Usage(); got != 0 {
		t.Fatalf("Usage after release = %d, want 0", got)
	}
}

// ---- T1（V3 接入）：baidupcs backend 插件（RegisterBackend）----

// TestNewBaidupcsBackend_FromVolumeExtra 从 volume.Extra 读配置构造 backend。
func TestNewBaidupcsBackend_FromVolumeExtra(t *testing.T) {
	t.Parallel()
	st := newFakeBaidupcsStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) { return st, nil }
	v := volume.Volume{
		Name:    "sys-baidu-1",
		Type:    "baidupcs",
		RootDir: t.TempDir(),
		Extra: map[string]any{
			"bduss":       "test-bduss",
			"baidu_root":  "/disk1",
			"binary_path": "",
		},
	}
	be, err := newBaidupcsBackendWithFactory(context.Background(), v, factory)
	if err != nil {
		t.Fatalf("newBaidupcsBackend: %v", err)
	}
	if be == nil {
		t.Fatal("backend 不应为 nil")
	}
	fs := be.FS()
	if fs == nil {
		t.Fatal("FS 不应为 nil")
	}
	// 用 FS 真跑一次 WriteFile（fake 内存网盘）→ 证明构造链路可用（Extra→Storage→StorageFS）。
	if writeErr := fs.WriteFile(context.Background(), "x.txt", bytes.NewReader([]byte("netdisk")), 7, 0); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}
	rc, openErr := fs.OpenRead(context.Background(), "x.txt")
	if openErr != nil {
		t.Fatalf("OpenRead: %v", openErr)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "netdisk" {
		t.Fatalf("内容 = %q, want %q", got, "netdisk")
	}
	// Close 幂等（无连接资源，返回 nil）。
	if err := be.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := be.Close(); err != nil {
		t.Fatalf("Close 二次: %v", err)
	}
}

// TestNewBaidupcsBackend_MissingCreds Extra 缺 bduss/binary_path → 明确错误（fail-closed）。
func TestNewBaidupcsBackend_MissingCreds(t *testing.T) {
	t.Parallel()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return newFakeBaidupcsStorage(), nil
	}
	v := volume.Volume{
		Name:    "sys-baidu-1",
		Type:    "baidupcs",
		RootDir: t.TempDir(),
		Extra:   map[string]any{}, // 无凭据
	}
	if _, err := newBaidupcsBackendWithFactory(context.Background(), v, factory); err == nil {
		t.Fatal("缺凭据应报错（fail-closed）")
	}
}

// TestRegisterBaidupcsBackend registry 分派：注册后 NewBackend 构造 baidupcs backend。
func TestRegisterBaidupcsBackend(t *testing.T) {
	t.Parallel()
	typ := "baidupcs-t1-dispatch"
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return newFakeBaidupcsStorage(), nil
	}
	registerBaidupcsBackendWithFactory(typ, factory)
	v := volume.Volume{
		Name:    "sys-baidu-1",
		Type:    typ,
		RootDir: t.TempDir(),
		Extra:   map[string]any{"bduss": "test-bduss"},
	}
	be, err := registry.NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("registry.NewBackend: %v", err)
	}
	if be == nil {
		t.Fatal("分派构造 backend 不应为 nil")
	}
	if be.FS() == nil {
		t.Fatal("FS 不应为 nil")
	}
}

// TestNewBaidupcsBackend_ExtraLocalRootWins 验证 backend 读 extra.local_root（优先于
// v.RootDir）作为本地中间态基目录——T7 的 local_root 语义迁移到 volumes[].extra 后，
// 配置的中间态目录必须生效（V3 框架外部卷 RootDir="" 不承载该语义）。
func TestNewBaidupcsBackend_ExtraLocalRootWins(t *testing.T) {
	t.Parallel()
	var gotTemp string
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		gotTemp = cfg.TempDir
		return newFakeBaidupcsStorage(), nil
	}
	fallback := filepath.Join(t.TempDir(), "fallback")   // RootDir 兜底（应被 local_root 覆盖）
	localRoot := filepath.Join(t.TempDir(), "localroot") // extra.local_root 优先
	v := volume.Volume{
		Name:    "sys-baidu-1",
		Type:    "baidupcs",
		RootDir: fallback, // 应被 extra.local_root 覆盖（V3 外部卷 RootDir 恒空）
		Extra: map[string]any{
			"bduss":      "test-bduss",
			"local_root": localRoot, // 优先
		},
	}
	if _, err := newBaidupcsBackendWithFactory(context.Background(), v, factory); err != nil {
		t.Fatalf("newBaidupcsBackend: %v", err)
	}
	if gotTemp != localRoot {
		t.Fatalf("TempDir = %q, want extra.local_root %q", gotTemp, localRoot)
	}
}

// TestSetupBaidupcsFactory_OwnerScope 验证工厂闭包按任务 owner 装配 per-owner quota：
// scopeFor(ownerA) 与 scopeFor(ownerB) 各自独立 Scope（配额分桶不串），ownerScope nil
// （该 owner 无配额）→ 不装配（StorageFS.quota 保持 nil，WriteFile 不受限）。
func TestSetupBaidupcsFactory_OwnerScope(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	st := newFakeBaidupcsStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) { return st, nil }
	set := setupBaidupcsTestSet(t, "baidupcs-p5-ownerscope", factory, "mydisk")

	poolA := quota.NewPool(100)
	poolB := quota.NewPool(100)
	// alice 上限 3（小额度，验证 staging 预留生效：WriteFile 5 字节应超限拒绝）；
	// bob 上限 100（正常，验证独立分桶不受 alice 影响）。
	scopeA := poolA.Scope("", 3) // 持有引用（工厂注入的同一个 Scope）
	scopeB := poolB.Scope("", 100)
	scopeFor := func(owner string) *quota.Scope {
		switch owner {
		case "alice":
			return scopeA
		case "bob":
			return scopeB
		default:
			return nil // 无配额 owner → 不装配
		}
	}
	setupBaidupcsFSFactory(exec, set, discardLoggerMain(), scopeFor)
	if exec.BaidupcsFS == nil {
		t.Fatal("装配 baidupcs 卷应注入工厂")
	}

	// ownerA 任务 → 工厂 → StorageFS WithQuota(scopeA)。
	fsA, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	}, "alice")
	if err != nil {
		t.Fatalf("工厂 ownerA: %v", err)
	}
	// WriteFile 触发 staging 预留 → scopeA 上限 3，5 字节应超限拒绝（quota per-owner 生效）。
	if werr := fsA.WriteFile(context.Background(), "a.txt", bytes.NewReader([]byte("hello")), 5, 0); werr == nil {
		t.Fatal("WriteFile(alice 5B) 应超限失败（scopeA 上限 3——per-owner quota 生效）")
	}
	if got := scopeA.Usage(); got != 0 {
		t.Fatalf("scopeA Usage = %d, want 0（预留失败未入账）", got)
	}
	if got := scopeB.Usage(); got != 0 {
		t.Fatalf("scopeB Usage = %d, want 0（bob 配额未受 alice 影响）", got)
	}

	// ownerB 任务 → 工厂 → StorageFS WithQuota(scopeB)。
	fsB, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	}, "bob")
	if err != nil {
		t.Fatalf("工厂 ownerB: %v", err)
	}
	if werr := fsB.WriteFile(context.Background(), "b.txt", bytes.NewReader([]byte("world")), 5, 0); werr != nil {
		t.Fatalf("WriteFile(bob): %v", werr)
	}
	// bob 上限 100：WriteFile 成功（预留→上传→释放，Usage 归 0——记账窗口在传输中）。
	if got := scopeB.Usage(); got != 0 {
		t.Fatalf("scopeB Usage = %d, want 0（bob 写入成功且释放）", got)
	}

	// 无配额 owner（无 scopeFor 命中）→ StorageFS.quota nil，WriteFile 不受限。
	fsC, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	}, "carol")
	if err != nil {
		t.Fatalf("工厂 ownerC: %v", err)
	}
	if werr := fsC.WriteFile(context.Background(), "c.txt", bytes.NewReader([]byte("data")), 4, 0); werr != nil {
		t.Fatalf("WriteFile(carol 无配额): %v", werr)
	}
}
