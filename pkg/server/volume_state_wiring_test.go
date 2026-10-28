// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_state_wiring_test.go 锁定卷状态缓存的**装配机制**（设计
// 2026-10-28-volume-state-maintenance §3.3）：
//  1. 唯一装配点 `wrapVolumeState` 只包**非本地卷**的 ViewUser 视图；
//  2. 包上之后目录列举命中缓存（第二次不回源 = 远端流量下降）；
//  3. 经装饰器的写入**自动失效**（无需调用方登记 —— #874 缺陷族的结构解）；
//  4. `disable: true`（cache=nil）时零行为变更。

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// countingFS 是记录 ListDir 次数的 fake FS（不含任何可选能力）。
type countingFS struct {
	mu     sync.Mutex
	lists  int
	writes int
}

func (f *countingFS) ListDir(_ context.Context, _ string) ([]syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return []syncpkg.Entry{{Path: "alice/user/a.txt", Size: 1}}, nil
}
func (f *countingFS) Stat(context.Context, string) (*syncpkg.Entry, error) { return nil, nil }
func (f *countingFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return nil, syncpkg.ErrUnsupported
}
func (f *countingFS) WriteFile(context.Context, syncpkg.LogicalKey, string, io.Reader, int64, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	return nil
}
func (f *countingFS) Rename(context.Context, string, string) error { return nil }
func (f *countingFS) Delete(context.Context, string) error         { return nil }
func (f *countingFS) MakeDir(context.Context, string) error        { return nil }

func (f *countingFS) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// fakeBE 是 ExternalBackend 最小实现。
type fakeBE struct{ fs syncpkg.FS }

func (b fakeBE) FS() syncpkg.FS { return b.fs }
func (b fakeBE) Close() error   { return nil }

// newStateCacheTestSet 构造「一个外部卷 + 一个本地卷」的卷集合。
func newStateCacheTestSet(t *testing.T, extFS syncpkg.FS) (*registry.Set, *storage.Root) {
	t.Helper()
	root, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	vs := registry.NewSet(
		[]volume.Volume{{Name: "local1"}, {Name: "ext1", Type: "fakeext"}},
		map[string]*storage.Root{"local1": root},
		map[string]registry.ExternalBackend{"ext1": fakeBE{fs: extFS}},
		nil, "local1",
	)
	// NewSet 已把 external 句柄登记进统一存储卷表（#872）⇒ VolumeView 可直接取到。
	return vs, root
}

// TestWrapVolumeState_CachesExternalVolumeList 钉住机制：外部卷二次 list 不回源。
func TestWrapVolumeState_CachesExternalVolumeList(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	vs, _ := newStateCacheTestSet(t, cfs)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.VolumeState.TTL = time.Minute

	cache := newVolumeStateCache(cfg, vs, testLogger())
	if cache == nil {
		t.Fatal("启用后应构造缓存")
	}
	wrapped := wrapVolumeState(cache, "ext1", registry.ViewUser, cfs)
	if wrapped == syncpkg.FS(cfs) {
		t.Fatal("外部卷 ViewUser 应被状态缓存包装")
	}
	for range 3 {
		if _, err := wrapped.ListDir(context.Background(), "alice/user"); err != nil {
			t.Fatalf("ListDir: %v", err)
		}
	}
	if got := cfs.listCalls(); got != 1 {
		t.Fatalf("三次 list 应只回源 1 次，got %d（装饰器未生效）", got)
	}
}

// TestWrapVolumeState_WriteInvalidatesWithoutRegistration 钉住 P3：
// 经装饰器的写入自动失效，调用方**不需要**任何登记（#874 结构解）。
func TestWrapVolumeState_WriteInvalidatesWithoutRegistration(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	vs, _ := newStateCacheTestSet(t, cfs)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.VolumeState.TTL = time.Minute

	cache := newVolumeStateCache(cfg, vs, testLogger())
	wrapped := wrapVolumeState(cache, "ext1", registry.ViewUser, cfs)

	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if got := cfs.listCalls(); got != 1 {
		t.Fatalf("首次 list 回源 1 次，got %d", got)
	}
	if err := wrapped.WriteFile(context.Background(), syncpkg.LogicalKey{}, "alice/user/b.txt",
		nil, 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, _ = wrapped.ListDir(context.Background(), "alice/user")
	if got := cfs.listCalls(); got != 2 {
		t.Fatalf("写入后应自动失效并回源（2 次），got %d（失效未自动发生）", got)
	}
}

// TestWrapVolumeState_LocalVolumeCached 钉住 §15.3 决策 (a)：本地卷同样纳入缓存
// （§15.2 步骤 2 的性能前提；写穿由 storage.Root 事件提供，TTL 只吸收进程外改动）。
func TestWrapVolumeState_LocalVolumeCached(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	vs, _ := newStateCacheTestSet(t, cfs)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cache := newVolumeStateCache(cfg, vs, testLogger())

	got := wrapVolumeState(cache, "local1", registry.ViewUser, cfs)
	if got == syncpkg.FS(cfs) {
		t.Fatal("本地卷也应被状态缓存包装（决策 a：本地卷参与缓存）")
	}
	for range 3 {
		if _, err := got.ListDir(context.Background(), "local1/user"); err != nil {
			t.Fatalf("ListDir: %v", err)
		}
	}
	if n := cfs.listCalls(); n != 1 {
		t.Fatalf("本地卷三次 list 应只回源 1 次，got %d（装饰器未生效）", n)
	}
}

// TestWrapVolumeState_DisabledNoop 钉住关闭语义：cache=nil 时原样返回（零回归）。
func TestWrapVolumeState_DisabledNoop(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	if got := wrapVolumeState(nil, "ext1", registry.ViewUser, cfs); got != syncpkg.FS(cfs) {
		t.Fatal("未启用时应原样返回")
	}
}

// TestVolumeStateTTL_Unified 钉住 §15.3 决策 (b)：本地卷与外部卷**同档 TTL**
// （本地卷写穿只覆盖本进程，进程外改动必须有收敛窗口；mtime 代际不能替代——POSIX 只更新直接父目录）。
func TestVolumeStateTTL_Unified(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()

	for _, vol := range []string{"local1", "ext1", ""} {
		if got := volumeStateTTL(cfg, vol); got != volumestate.DefaultTTL {
			t.Fatalf("%q 默认 TTL 应为 DefaultTTL(%v)，got %v", vol, volumestate.DefaultTTL, got)
		}
	}
	cfg.VolumeState.TTL = 5 * time.Second
	if got := volumeStateTTL(cfg, "local1"); got != 5*time.Second {
		t.Fatalf("显式 TTL 应全卷统一，got %v", got)
	}
}

// TestExternalFSFor_UsesStateCache 钉住**生产路径**：`Handlers.externalFSFor`（即所有外部卷
// 读/写面的唯一解析出口）在启用后自带缓存，二次列举不回源。
func TestExternalFSFor_UsesStateCache(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	vs, _ := newStateCacheTestSet(t, cfs)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.VolumeState.TTL = time.Minute

	cache := newVolumeStateCache(cfg, vs, testLogger())
	// 与 RegisterRoutes 相同的注册方式（唯一装配点）。
	vs.SetVolumeViewDecorator(func(name string, use registry.ViewUse, raw syncpkg.FS) syncpkg.FS {
		return wrapVolumeState(cache, name, use, raw)
	})
	h := &Handlers{volSet: vs, logger: testLogger(), stateCache: cache}

	fs1, ok := h.externalFSFor("ext1")
	if !ok || fs1 == nil {
		t.Fatal("externalFSFor 应返回卷视图")
	}
	_, _ = fs1.ListDir(context.Background(), "alice/user")
	if _, ok := h.externalFSFor("ext1"); !ok {
		t.Fatal("第二次解析应成功")
	}
	if got := cfs.listCalls(); got != 1 {
		t.Fatalf("经 externalFSFor 的二次访问应命中缓存（1 次回源），got %d", got)
	}
}

// TestNewVolumeStateCache_DefaultEnabledAndDisable 钉住「缓存默认开启」（AGENTS.md 7c：
// 需要默认开启时把语义反转为 disable_*，bool 默认 false）。
func TestNewVolumeStateCache_DefaultEnabledAndDisable(t *testing.T) {
	t.Parallel()
	cfs := &countingFS{}
	vs, _ := newStateCacheTestSet(t, cfs)
	cfg := Default()
	cfg.StorageRoot = t.TempDir()

	if cache := newVolumeStateCache(cfg, vs, testLogger()); cache == nil {
		t.Fatal("默认应启用卷状态缓存（disable=false）")
	}
	cfg.VolumeState.Disable = true
	if cache := newVolumeStateCache(cfg, vs, testLogger()); cache != nil {
		t.Fatal("disable=true 时应关闭缓存（零回归路径）")
	}
}
