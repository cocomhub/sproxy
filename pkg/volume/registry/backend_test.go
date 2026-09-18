// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// fakeExternal 是测试用 ExternalBackend 最小实现（FS 返回 fakeFS；Close 计数）。
type fakeExternal struct {
	fs    syncpkg.FS
	close bool
}

func (f *fakeExternal) FS() syncpkg.FS { return f.fs }
func (f *fakeExternal) Close() error   { f.close = true; return nil }

// fakeFS 是最小 sync.FS（仅编译期断言；无测试走到其方法）。
type fakeFS struct{}

func (fakeFS) ListDir(context.Context, string) ([]syncpkg.Entry, error) { return nil, nil }
func (fakeFS) Stat(context.Context, string) (*syncpkg.Entry, error)     { return nil, nil }
func (fakeFS) OpenRead(context.Context, string) (io.ReadCloser, error)  { return nil, nil }
func (fakeFS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return nil
}
func (fakeFS) Rename(context.Context, string, string) error { return nil }
func (fakeFS) Delete(context.Context, string) error         { return nil }
func (fakeFS) MakeDir(context.Context, string) error        { return nil }

// unregisterBackendForTest 移除测试注册的后端（清理，防跨用例污染注册表）。
func unregisterBackendForTest(typ string) {
	backendMu.Lock()
	defer backendMu.Unlock()
	delete(backendFactories, typ)
}

// TestRegisterBackend_Dispatch 钉住后端注册 + 按 Type 分派：注册 fake 构造器后，
// NewBackend(ctx, v) 用 v.Type 命中并构造 ExternalBackend（从 v.Extra 读配置）。
func TestRegisterBackend_Dispatch(t *testing.T) {
	t.Parallel()
	const typ = "fake-dispatch"
	calls := 0
	RegisterBackend(typ, func(_ context.Context, v volume.Volume) (ExternalBackend, error) {
		calls++
		got, _ := v.Extra["key"].(string)
		if got != "val" {
			return nil, errors.New("Extra.key 未透传")
		}
		return &fakeExternal{fs: fakeFS{}}, nil
	})
	defer unregisterBackendForTest(typ)

	v := volume.Volume{Name: "v1", Type: typ, Extra: map[string]any{"key": "val"}}
	be, err := NewBackend(context.Background(), v)
	if err != nil {
		t.Fatalf("NewBackend(%q): %v", typ, err)
	}
	if be == nil {
		t.Fatal("NewBackend 返回 nil，want 非 nil")
	}
	if calls != 1 {
		t.Fatalf("构造器被调 %d 次，want 1", calls)
	}
	_ = be.Close()
}

// TestRegisterBackend_UnknownType 钉住未注册 Type → 明确错误（fail-closed，不回落）。
func TestRegisterBackend_UnknownType(t *testing.T) {
	t.Parallel()
	_, err := NewBackend(context.Background(), volume.Volume{Name: "v1", Type: "no-such-backend"})
	if err == nil {
		t.Fatal("未注册 Type 应返回错误，got nil")
	}
	if !strings.Contains(err.Error(), "no-such-backend") {
		t.Fatalf("错误应包含未注册类型名，got %v", err)
	}
}

// TestRegisterBackend_DuplicateType 钉住重复注册 → panic（编程错误，仿标准库 Register 语义）。
func TestRegisterBackend_DuplicateType(t *testing.T) {
	t.Parallel()
	defer unregisterBackendForTest("fake-dup")
	RegisterBackend("fake-dup", func(context.Context, volume.Volume) (ExternalBackend, error) {
		return &fakeExternal{}, nil
	})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("重复注册应 panic，got 无 panic")
		}
	}()
	RegisterBackend("fake-dup", func(context.Context, volume.Volume) (ExternalBackend, error) {
		return &fakeExternal{}, nil
	})
}

// TestSet_ExternalBackend 钉住 NewSet 带 external → External(name) 可取（FS 返回非 nil）。
func TestSet_ExternalBackend(t *testing.T) {
	t.Parallel()
	ext := &fakeExternal{fs: fakeFS{}}
	set := NewSet(
		[]volume.Volume{{Name: "ext1", Type: "fake"}},
		map[string]*storage.Root{},
		map[string]ExternalBackend{"ext1": ext},
		map[string]*quota.Pool{"ext1": quota.NewPool(0)},
		"ext1",
	)
	t.Cleanup(func() { _ = set.Close() })
	got := set.External("ext1")
	if got == nil {
		t.Fatal("External(ext1) = nil, want 非 nil")
	}
	if got.FS() == nil {
		t.Fatal("External(ext1).FS() = nil, want 非 nil")
	}
}

// TestSet_External_UnknownName 钉住未知卷名 → nil。
func TestSet_External_UnknownName(t *testing.T) {
	t.Parallel()
	set := NewSet(nil, map[string]*storage.Root{}, nil, map[string]*quota.Pool{}, "default")
	t.Cleanup(func() { _ = set.Close() })
	if set.External("不存在") != nil {
		t.Fatal("External(未知卷名) 应返回 nil")
	}
}

// TestSet_Close_ClosesExternal 钉住 Close 关闭 external（幂等）。
func TestSet_Close_ClosesExternal(t *testing.T) {
	t.Parallel()
	ext := &fakeExternal{fs: fakeFS{}}
	set := NewSet(
		[]volume.Volume{{Name: "ext1", Type: "fake"}},
		map[string]*storage.Root{},
		map[string]ExternalBackend{"ext1": ext},
		map[string]*quota.Pool{"ext1": quota.NewPool(0)},
		"ext1",
	)
	if err := set.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if !ext.close {
		t.Fatal("Close() 未关闭 external")
	}
	if set.External("ext1") != nil {
		t.Fatal("Close 后 External(ext1) 应返回 nil（external map 已清空）")
	}
}

// TestBackendTypes 钉住 BackendTypes 导出已注册类型列表（V4：backend 列表 API 数据源）。
// 注册唯一 fake 类型 → 列表含它（已注册集合的超集断言——列表可含其它测试/装配注册的类型，
// 只验证「新增的必在」）。
func TestBackendTypes(t *testing.T) {
	t.Parallel()
	const typ = "fake-types"
	RegisterBackend(typ, func(_ context.Context, v volume.Volume) (ExternalBackend, error) {
		return &fakeExternal{}, nil
	})
	defer unregisterBackendForTest(typ)

	types := BackendTypes()
	if len(types) == 0 {
		t.Fatal("BackendTypes 返回空列表，want 至少含已注册类型")
	}
	found := slices.Contains(types, typ)
	if !found {
		t.Fatalf("BackendTypes 应含 %q，got %v", typ, types)
	}
}

// ---- C1：VolumeStatsProvider 接口（外部卷总量查询）----

// statsBackend 是带 Stats 的 fake backend（C1 测试）。
type statsBackend struct {
	fs  syncpkg.FS
	sts *VolumeStats
}

func (b *statsBackend) FS() syncpkg.FS { return b.fs }
func (b *statsBackend) Close() error   { return nil }
func (b *statsBackend) Stats(_ context.Context) (*VolumeStats, error) {
	return b.sts, nil
}

// TestVolumeStatsProvider_Optional 验证：实现了 VolumeStatsProvider 的 backend 可被
// 查询返回 Stats；未实现的 backend Stats 返回 nil（WebDAV 仅限额维度）。
func TestVolumeStatsProvider_Optional(t *testing.T) {
	t.Parallel()
	sts := &VolumeStats{TotalBytes: 100, UsedBytes: 40}
	be := &statsBackend{fs: fakeFS{}, sts: sts}
	var p VolumeStatsProvider = be // 编译期断言：statsBackend 实现接口
	got, err := p.Stats(context.Background())
	if err != nil || got != sts {
		t.Fatalf("Stats = %v/%v, want %v/nil", got, err, sts)
	}
	// 未实现 Stats 的 backend（如普通 fakeExternal）→ 类型断言失败（WebDAV 语义）。
	var plain ExternalBackend = &fakeExternal{fs: fakeFS{}}
	if _, ok := plain.(VolumeStatsProvider); ok {
		t.Fatal("无 Stats 的 backend 不应实现 VolumeStatsProvider")
	}
}
