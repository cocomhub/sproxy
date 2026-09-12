// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// volume_read_test.go 补齐多卷**读定位**此前无包内覆盖的两处能力：
//   - `volumeFileExists`：卷内文件存在性探测（fail-closed：卷未知 → 未命中，而非报错）；
//   - `Service.locateForRead` 的**显式卷**分支：`?volume=` 只在指定卷内定位（未知卷 /
//     卷上不存在 → 未命中，不泄露卷存在性），以及「未装配卷集合时显式卷直接未命中」。
//
// 单卷 / 全视图分支（Deps.LocateOwnerFile）已由写面与只读面用例覆盖，本文件不重复。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVolumeFileExists_FailClosed 覆盖卷内存在性探测的三条结果：
// 命中 → true；卷名未知（不在集合）→ (false, nil) 不报错；卷上不存在 → (false, nil)。
func TestVolumeFileExists_FailClosed(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	disk2User := filepath.Join(env.volDirs["disk2"], "alice", "user")
	if err := os.MkdirAll(disk2User, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(disk2User, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	vs := env.svc.rt.volSet()
	if vs == nil {
		t.Fatal("多卷环境应注入 VolSet")
	}
	if exists, err := volumeFileExists(vs, "disk2", "alice", "user/a.txt"); err != nil || !exists {
		t.Fatalf("命中应为 (true, nil), got (%v, %v)", exists, err)
	}
	if exists, err := volumeFileExists(vs, "nope", "alice", "user/a.txt"); err != nil || exists {
		t.Fatalf("未知卷应 fail-closed 为 (false, nil), got (%v, %v)", exists, err)
	}
	if exists, err := volumeFileExists(vs, "disk2", "alice", "user/missing.txt"); err != nil || exists {
		t.Fatalf("卷上不存在应为 (false, nil), got (%v, %v)", exists, err)
	}
}

// TestService_LocateForRead_ExplicitVolume 覆盖显式卷定位分支：
// 命中返回卷名与租户；未知卷名 / 卷上无此文件 → 未命中（fail-closed，不泄露卷存在性）。
func TestService_LocateForRead_ExplicitVolume(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	disk2User := filepath.Join(env.volDirs["disk2"], "alice", "user")
	if err := os.MkdirAll(disk2User, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(disk2User, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loc, ok := env.svc.locateForRead("alice", "user/a.txt", "disk2")
	if !ok || loc.VolumeName != "disk2" || loc.Tenant == nil {
		t.Fatalf("显式卷命中应返回 {disk2, tenant}, got (%+v, %v)", loc, ok)
	}
	if _, ok := env.svc.locateForRead("alice", "user/a.txt", "nope"); ok {
		t.Fatal("未知卷名应未命中（不泄露卷存在性）")
	}
	if _, ok := env.svc.locateForRead("alice", "user/missing.txt", "disk2"); ok {
		t.Fatal("卷上无此文件应未命中")
	}
}

// TestService_LocateForRead_NoVolSetExplicitVolume 覆盖未装配卷集合（单卷旧装配）时
// 显式卷定位直接未命中——不得因无卷集合而回落默认租户（否则 `?volume=` 变成越权读）。
func TestService_LocateForRead_NoVolSetExplicitVolume(t *testing.T) {
	env := newDirsEnv(t)
	if env.svc.rt.volSet() != nil {
		t.Fatal("单卷环境 VolSet 应为 nil")
	}
	if _, ok := env.svc.locateForRead("alice", "user/a.txt", "any"); ok {
		t.Fatal("未装配卷集合时显式卷应未命中")
	}
}

// TestService_LocateForRead_ViewLocate 覆盖非显式卷（全视图）分支：委托 Deps.LocateOwnerFile。
func TestService_LocateForRead_ViewLocate(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")
	env.locateOwnerFile = env.locateOwnerFileDefault
	env.rebuild()

	writeUserFile(t, env, "alice", "user/a.txt", "A")
	loc, ok := env.svc.locateForRead("alice", "user/a.txt", "")
	if !ok || loc.VolumeName != "main" || loc.Tenant == nil {
		t.Fatalf("全视图命中应返回默认卷租户, got (%+v, %v)", loc, ok)
	}
	if _, ok := env.svc.locateForRead("alice", "user/missing.txt", ""); ok {
		t.Fatal("全视图未命中应返回 false")
	}
}
