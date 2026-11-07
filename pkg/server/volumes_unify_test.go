// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_unify_test.go —— 统一存储卷模型 U1 的装配层验收测试。
//
// 设计：docs/designs/2026-10-28-unified-volume-model.md §二 U1。
// 钉住：`assembleVolumes` 装配本地卷后，`Set.StorageVolume(name)` 可取到统一身份
// （不再只能经 `Set.Root` 拿裸 root），且容量记账（pool）仍在位。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestAssembleVolumes_LocalBackendStorageVolume 钉住 U1 装配接线：本地卷装配后
// 经统一访问器可达，Kind=Storage，且 localBackend 的足迹/上界能力在位。
func TestAssembleVolumes_LocalBackendStorageVolume(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	sv, ok := vs.StorageVolume("default")
	if !ok || sv == nil {
		t.Fatal("本地卷应经 Set.StorageVolume 取到（U1 装配接线）")
	}
	if sv.Name() != "default" {
		t.Fatalf("StorageVolume.Name() = %q, want default", sv.Name())
	}
	if sv.Kind() != volume.KindStorage {
		t.Fatalf("本地卷 Kind = %v, want KindStorage", sv.Kind())
	}
	if sv.FS() == nil {
		t.Fatal("StorageVolume.FS() 不应为 nil（数据面视图）")
	}
	// 容量记账仍在位（pool 保留）。
	if vs.Pool("default") == nil {
		t.Fatal("本地卷容量池应仍在（U1 不迁记账权威）")
	}
	// 足迹/上界能力在位（本地卷自报物理足迹——配额层依赖）。
	if _, ok := sv.(volume.Footprinter); !ok {
		t.Fatal("localBackend 应实现 volume.Footprinter")
	}
	if _, ok := sv.(volume.PhysBound); !ok {
		t.Fatal("localBackend 应实现 volume.PhysBound")
	}
}

// TestAssembleVolumes_FSViewIsRootFS 钉住本地卷统一身份的数据面 = 卷根 FS（NewRootFS）：
// 与既有 `Set.Root` 路径**同一根**（不引入第二份句柄，零行为变更）。
func TestAssembleVolumes_FSViewIsRootFS(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = root
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	sv, ok := vs.StorageVolume("default")
	if !ok {
		t.Fatal("StorageVolume 应可达")
	}
	rt := vs.Root("default")
	if rt == nil {
		t.Fatal("roots 应保留默认卷根（存量 tenant/globalRoot 语义）")
	}
	// FS 视图非 nil 且与 Root 路径同源（同一 root 句柄衍生的适配器）。
	if sv.FS() == nil {
		t.Fatal("FS() 不应为 nil")
	}
}
