// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// capacity_test.go 钉住 C2 卷级计数记账：写入累计 / 删除释放 / 超限拒绝 / 持久化。
//
// 用户定案（2026-09-18）：卷级计数记账——外部卷写入累计 + 删除释放，超限拒绝；
// 本系统可用限额（UserVolume.Capacity）强制生效。

// nopCounter 是 Counter 的空实现（能力透传测试不关心记账）。
type nopCounter struct{}

func (nopCounter) TryAdd(int64) error { return nil }
func (nopCounter) Release(int64)      {}
func (nopCounter) Used() int64        { return 0 }
func (nopCounter) Capacity() int64    { return 1 << 20 }
func (nopCounter) Flush() error       { return nil }

// stubSettingsBackend 是同时实现 Settings 三能力的假后端（能力透传测试用）。
type stubSettingsBackend struct {
	registry.ExternalBackend
	calls int
}

func (s *stubSettingsBackend) FS() syncpkg.FS { return nil }
func (s *stubSettingsBackend) Close() error   { return nil }
func (s *stubSettingsBackend) Settings(context.Context) ([]registry.VolumeSettings, error) {
	s.calls++
	return []registry.VolumeSettings{{Key: "block_policy"}}, nil
}
func (s *stubSettingsBackend) UpdateSettings(context.Context, string, json.RawMessage) error {
	return nil
}
func (s *stubSettingsBackend) SetSettingsStore(registry.SettingsStore) error { return nil }

// plainBackend 只实现基础 ExternalBackend。
type plainBackend struct{}

func (plainBackend) FS() syncpkg.FS { return nil }
func (plainBackend) Close() error   { return nil }

// TestBackend_SettingsCapabilityPassthrough 锁定 R21 式能力透传：容量包装器必须转发
// 卷设置三能力到 inner —— 否则装配层（只拿得到包装器）断言恒失败，
// 「卷详情/设置分区」在真实装配路径上整块失效（审查 Critical）。
func TestBackend_SettingsCapabilityPassthrough(t *testing.T) {
	t.Parallel()
	inner := &stubSettingsBackend{}
	var wrapped registry.ExternalBackend = WrapBackend(inner, nopCounter{})
	sp, ok := wrapped.(registry.SettingsProvider)
	if !ok {
		t.Fatal("容量包装器未透传 SettingsProvider（装配层断言会恒失败）")
	}
	if _, err := sp.Settings(context.Background()); err != nil {
		t.Fatalf("Settings 透传失败: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("应委托 inner 一次，得到 %d", inner.calls)
	}
	if _, ok := wrapped.(registry.SettingsWriter); !ok {
		t.Fatal("容量包装器未透传 SettingsWriter")
	}
	if _, ok := wrapped.(registry.SettingsStoreSetter); !ok {
		t.Fatal("容量包装器未透传 SettingsStoreSetter")
	}
}

// TestBackend_SettingsUnsupportedInner 锁定 inner 不支持时返回 ErrUnsupported
// （调用方据此判「该卷无可调配置」，而不是 500）。
func TestBackend_SettingsUnsupportedInner(t *testing.T) {
	t.Parallel()
	var wrapped registry.ExternalBackend = WrapBackend(plainBackend{}, nopCounter{})
	sp, ok := wrapped.(registry.SettingsProvider)
	if !ok {
		t.Fatal("包装器应恒实现能力接口（用 ErrUnsupported 表达未实现）")
	}
	if _, err := sp.Settings(context.Background()); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("inner 未实现时应返回 ErrUnsupported，得到 %v", err)
	}
}

// reloadableInner 记录 ReloadSettings 调用（能力透传测试用）。
type reloadableInner struct {
	plainBackend
	reloadCalls int
	reloadErr   error
}

func (r *reloadableInner) ReloadSettings(context.Context) error {
	r.reloadCalls++
	return r.reloadErr
}

// TestBackend_SettingsReloaderPassthrough 锁定 `SettingsReloader` 也必须透传（安全审查）：
// 生产装配路径上 `Set.External(卷名)` 拿到的是容量包装器；若包装器不实现该能力，
// refresh 会落回**吞错的 Setter 兼容路径** ⇒ 「重载失败可见」在生产上等于没修。
func TestBackend_SettingsReloaderPassthrough(t *testing.T) {
	t.Parallel()
	inner := &reloadableInner{}
	var wrapped registry.ExternalBackend = WrapBackend(inner, nopCounter{})
	rl, ok := wrapped.(registry.SettingsReloader)
	if !ok {
		t.Fatal("容量包装器未透传 SettingsReloader（refresh 会落回吞错路径）")
	}
	if err := rl.ReloadSettings(context.Background()); err != nil {
		t.Fatalf("透传失败: %v", err)
	}
	if inner.reloadCalls != 1 {
		t.Fatalf("应委托 inner 一次，得到 %d", inner.reloadCalls)
	}
	// inner 未实现 ⇒ ErrUnsupported（调用方据此判「无重载语义」，而不是当作成功）
	var wrapped2 registry.ExternalBackend = WrapBackend(plainBackend{}, nopCounter{})
	rl2, ok2 := wrapped2.(registry.SettingsReloader)
	if !ok2 {
		t.Fatal("包装器应恒实现能力接口（用 ErrUnsupported 表达未实现）")
	}
	if err := rl2.ReloadSettings(context.Background()); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("inner 未实现时应返回 ErrUnsupported，得到 %v", err)
	}
}

// TestWrapStorageVolume_SettingsReloaderReachable 经**真实装配入口** WrapStorageVolume 断言
// 能力可达（安全审查建议：裸 fake 测不出包装器遗漏）。
func TestWrapStorageVolume_SettingsReloaderReachable(t *testing.T) {
	t.Parallel()
	inner := &reloadableInner{}
	wrapped := WrapStorageVolume("v", inner, nil, nil, 1<<20, 0)
	if wrapped == nil {
		t.Fatal("WrapStorageVolume 返回 nil")
	}
	rl, ok := wrapped.(registry.SettingsReloader)
	if !ok {
		t.Fatal("经 WrapStorageVolume 后 SettingsReloader 不可达（refresh 会落回吞错路径）")
	}
	if err := rl.ReloadSettings(context.Background()); err != nil {
		t.Fatalf("重载透传失败: %v", err)
	}
	if _, ok := wrapped.(registry.SettingsProvider); !ok {
		t.Fatal("经 WrapStorageVolume 后 SettingsProvider 不可达")
	}
	if _, ok := wrapped.(registry.SettingsWriter); !ok {
		t.Fatal("经 WrapStorageVolume 后 SettingsWriter 不可达")
	}
	if _, ok := wrapped.(registry.SettingsStoreSetter); !ok {
		t.Fatal("经 WrapStorageVolume 后 SettingsStoreSetter 不可达")
	}
}
