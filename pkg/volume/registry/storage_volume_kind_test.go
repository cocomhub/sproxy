// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

// storage_volume_kind_test.go —— 统一存储卷模型 M0/M1 的**接缝测试**。
//
// 设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。
// 本片（M0）只定义**形状**（`pkg/volume/storage_volume.go`：`Kind`/`StorageVolume`/可选能力）
// 与**分类入口**（`registry.KindOf`），**零行为变更**：不改动任何现有装配路径。
//
// 覆盖两点：
//  1. `KindOf` 分类 —— 判据**单源**在本注册表的 wrapper 事实源（静态基线 secrets/secretdata/
//     egress + 处理层动态登记 `MarkWrapperType`），不重复任何类型清单；
//  2. **接缝可行性** —— 同一类型可**同时**满足 `registry.ExternalBackend` 与
//     `volume.StorageVolume`（M1「把 `local` 注册为普通后端」的前提 ✓）。
//
// 变异：`KindOf` 恒返回 `KindStorage` ⇒ 封装类型断言红；恒返回 `KindWrapper` ⇒ 本地/外部断言红。

import (
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestKindOf_Classification 钉住分类：本地与外部后端 ⇒ Storage；封装类型 ⇒ Wrapper。
func TestKindOf_Classification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		typ  string
		want volume.Kind
	}{
		{"空串（= 本地缺省）", "", volume.KindStorage},
		{"显式 local", volume.TypeLocal, volume.KindStorage},
		{"外部后端类型", "baidupcs", volume.KindStorage},
		{"封装基线：secretdata", "secretdata", volume.KindWrapper},
		{"封装基线：secrets", "secrets", volume.KindWrapper},
		{"封装基线：egress", "egress", volume.KindWrapper},
		{"未注册类型（防御性）", "no-such-backend-type", volume.KindStorage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := KindOf(volume.Volume{Name: "v", Type: tc.typ}); got != tc.want {
				t.Fatalf("KindOf(%q) = %v, want %v", tc.typ, got, tc.want)
			}
		})
	}
}

// TestKindOf_DynamicWrapperRegistration 钉住**动态登记**被 `KindOf` 采纳（处理层注册表投影）：
// 登记前 Storage、登记后 Wrapper。清理用 `UnmarkWrapperTypeForTest`（防污染共享投影表）。
func TestKindOf_DynamicWrapperRegistration(t *testing.T) {
	const typ = "kindof-test-wrapper-type"
	if got := KindOf(volume.Volume{Type: typ}); got != volume.KindStorage {
		t.Fatalf("登记前应为 KindStorage, got %v", got)
	}
	MarkWrapperType(typ)
	t.Cleanup(func() { UnmarkWrapperTypeForTest(typ) })
	if got := KindOf(volume.Volume{Type: typ}); got != volume.KindWrapper {
		t.Fatalf("登记后应为 KindWrapper, got %v", got)
	}
}

// fakeStorageVolume 是**接缝证明**用的最小实现：同一类型同时满足两个接口。
type fakeStorageVolume struct {
	name string
	fs   syncpkg.FS
}

func (f fakeStorageVolume) Name() string      { return f.name }
func (f fakeStorageVolume) Kind() volume.Kind { return volume.KindStorage }
func (f fakeStorageVolume) FS() syncpkg.FS    { return f.fs }
func (f fakeStorageVolume) Close() error      { return nil }

// 编译期：M1 要求「注册后端」与「统一存储卷」可被同一类型满足（否则迁移不可行）。
var (
	_ StorageVolume   = fakeStorageVolume{}
	_ ExternalBackend = fakeStorageVolume{}
)

// TestStorageVolumeSeam_CompatibleWithExternalBackend 运行期验证接缝形状可用。
func TestStorageVolumeSeam_CompatibleWithExternalBackend(t *testing.T) {
	t.Parallel()
	fs := syncpkg.NewLocalFS(t.TempDir(), nil)
	var sv StorageVolume = fakeStorageVolume{name: "vol1", fs: fs}
	be, ok := sv.(ExternalBackend)
	if !ok {
		t.Fatal("同一类型应同时满足 volume.StorageVolume 与 registry.ExternalBackend")
	}
	if sv.Name() != "vol1" || be.FS() == nil {
		t.Fatalf("接缝形状不可用: name=%q fs=%v", sv.Name(), be.FS())
	}
	if sv.Kind() != volume.KindStorage {
		t.Fatalf("Kind = %v, want KindStorage", sv.Kind())
	}
	if err := sv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
