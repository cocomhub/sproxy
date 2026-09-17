// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// baidupcs_sync_test.go 钉住 P4 **baidupcs 载体装配**的行为：
//   - 未启用 → 不注入工厂（kind=baidupcs 远端 fail-closed）；
//   - 启用 + fake 工厂 → 工厂注入，按 remote.Volume 查返回 StorageFS，push/pull 可跑；
//   - 工厂返回错误 → 不注入（fail-closed，绝不回落 direct）；
//   - quota 适配器（scopeQuotaTracker）：ReserveUsage = TryReserve+Commit（计数器入账）、
//     ReleaseUsage = ReleaseUsage（扣减），对齐 P2 Quota 语义。

import (
	"bytes"
	"context"
	"io"
	"testing"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
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

// baidupcsCfg 返回启用 baidupcs 的配置（单盘）。
func baidupcsCfg(t *testing.T, enabled bool) *server.Config {
	t.Helper()
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Baidupcs.Enabled = enabled
	if enabled {
		cfg.Baidupcs.Disks = []server.BaidupcsDiskConfig{{
			Name: "mydisk", BDUSS: "test-bduss",
		}}
	}
	return cfg
}

// TestSetupBaidupcsFSFactory_Disabled 未启用 → 不注入工厂。
func TestSetupBaidupcsFSFactory_Disabled(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	cfg := baidupcsCfg(t, false)
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), nil)
	if exec.BaidupcsFS != nil {
		t.Fatal("未启用时不应注入 BaidupcsFS 工厂")
	}
}

// TestSetupBaidupcsFSFactory_Enabled 启用 + fake 工厂 → 注入，按 volume 查返回 FS。
func TestSetupBaidupcsFSFactory_Enabled(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	cfg := baidupcsCfg(t, true)
	cfg.SyncRemotes = []server.SyncRemoteConfig{{
		Name: "r-bd", Kind: "baidupcs", Volume: "mydisk",
	}}
	st := newFakeBaidupcsStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) { return st, nil }
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS == nil {
		t.Fatal("启用 + fake 工厂应注入 BaidupcsFS 工厂")
	}
	fs, closeFn, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	})
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
	// 用 FS 真跑一次 WriteFile（fake 内存网盘）→ 证明装配链路可用。
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

// TestSetupBaidupcsFSFactory_VolumeMissing 启用 + 工厂注入，但 remote.Volume 未装配 → 明确错误。
func TestSetupBaidupcsFSFactory_VolumeMissing(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	cfg := baidupcsCfg(t, true)
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return newFakeBaidupcsStorage(), nil
	}
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS == nil {
		t.Fatal("应注入工厂")
	}
	_, _, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "nonexistent",
	})
	if err == nil {
		t.Fatal("未装配卷应报错（fail-closed）")
	}
}

// TestSetupBaidupcsFSFactory_StorageError 工厂返回错误 → 不注入（fail-closed）。
func TestSetupBaidupcsFSFactory_StorageError(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	cfg := baidupcsCfg(t, true)
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		return nil, baidupcs.ErrInvalidParam
	}
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS != nil {
		t.Fatal("工厂失败时不应注入（fail-closed）")
	}
}

// TestSetupBaidupcsFSFactory_MultiDisk 多盘装配：2 盘 → 工厂按 remote.Volume 查对（disk1 → 盘1 的 FS）。
// 同时验证单盘构造失败不影响其余盘（部分失败容忍）。
func TestSetupBaidupcsFSFactory_MultiDisk(t *testing.T) {
	t.Parallel()
	exec := syncexec.NewExecutor(nil, nil)
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Baidupcs.Enabled = true
	cfg.Baidupcs.Disks = []server.BaidupcsDiskConfig{
		{Name: "disk1", BDUSS: "bduss-1"},
		{Name: "disk2", BDUSS: "bduss-2"},
	}
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
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS == nil {
		t.Fatal("多盘装配应注入 BaidupcsFS 工厂")
	}
	// 按 volume 查对：disk1 → 盘1 的 FS（写盘1 → 盘2 不可见）。
	fs1, close1, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk1",
	})
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
	})
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
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Baidupcs.Enabled = true
	cfg.Baidupcs.Disks = []server.BaidupcsDiskConfig{
		{Name: "disk1", BDUSS: "bduss-1"},
		{Name: "disk2", BDUSS: "bduss-2"},
	}
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
		if cfg.BDUSS == "bduss-2" {
			return nil, baidupcs.ErrInvalidParam
		}
		return newFakeBaidupcsStorage(), nil
	}
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS == nil {
		t.Fatal("部分失败仍应注入工厂（盘1 可用）")
	}
	// 盘1 可用。
	fs1, close1, err := exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk1",
	})
	if err != nil {
		t.Fatalf("盘1 应可用: %v", err)
	}
	close1()
	if fs1 == nil {
		t.Fatal("盘1 工厂应返回非 nil FS")
	}
	// 盘2 未装配 → 明确报错。
	_, _, err = exec.BaidupcsFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r2", Kind: syncmgr.RemoteKindBaidupcs, Volume: "disk2",
	})
	if err == nil {
		t.Fatal("构造失败的盘应报「卷未装配」（fail-closed）")
	}
}

// TestScopeQuotaTracker 适配器计数器语义：预留入账 committed，释放扣减。
func TestScopeQuotaTracker(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(100)
	scope := pool.Scope("", 0)
	q := &scopeQuotaTracker{scope: scope}
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
