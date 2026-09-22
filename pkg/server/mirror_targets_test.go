// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mirror_targets_test.go 验证多副本镜像（roadmap 3.3 P2 多副本演进）：
//  1. volumes[].mirror_targets 多目标：一个源卷 → N 个目标卷（周期 pass 逐目标复制）。
//  2. 单 mirror_to 与 mirror_targets 兼容（单目标等价；不双计）。
//  3. Config Validate：mirror_targets 重复目标 / 指向自身 / 不存在 / 成环 → 拒绝。

import (
	"net/http"
	"testing"
)

// TestVolumesMirror_MultiTargets 源卷 mirror_targets 两个目标 → pass 复制到两目标。
func TestVolumesMirror_MultiTargets(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20, MirrorTargets: []string{"r1", "r2"}},
		{Name: "r1", Root: dirs[1], VolCapacity: 1 << 20},
		{Name: "r2", Root: dirs[2], VolCapacity: 1 << 20},
	}
	url, h, _ := newVolumesAPIServer(t, "alice", volumes, nil)

	status, _, respBody := volumeUpload(t, url, "a.txt", []byte("replica-a"), "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}

	// 执行一轮镜像 pass（逐目标复制）。
	h.volumeMirrorPass()

	for i, dir := range []int{1, 2} {
		if !diskFileExists(t, dirs[dir], "alice", "a.txt") {
			t.Fatalf("mirror_targets 后 a.txt 应落目标 %d", i+1)
		}
		if got := string(readDiskFile(t, dirs[dir], "alice", "a.txt")); got != "replica-a" {
			t.Fatalf("目标 %d 内容=%q want replica-a", i+1, got)
		}
	}
	if !diskFileExists(t, dirs[0], "alice", "a.txt") {
		t.Fatal("mirror 后源应保留")
	}
}

// TestVolumesMirror_CompatSingleTo 单 mirror_to 与 mirror_targets 等价（不双计）。
func TestVolumesMirror_CompatSingleTo(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir()}
	volumes := []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 1 << 20, MirrorTo: "disk2"},
		{Name: "disk2", Root: dirs[1], VolCapacity: 1 << 20},
	}
	url, h, _ := newVolumesAPIServer(t, "alice", volumes, nil)

	status, _, respBody := volumeUpload(t, url, "a.txt", []byte("compat"), "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}
	h.volumeMirrorPass()
	if !diskFileExists(t, dirs[1], "alice", "a.txt") {
		t.Fatal("单 mirror_to 应仍工作（兼容）")
	}
}

// TestVolumesMirror_ValidateMultiTarget 配置校验：互斥 / 重复 / 自身 / 不存在。
func TestVolumesMirror_ValidateMultiTarget(t *testing.T) {
	t.Parallel()
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	base := []VolumeConfig{
		{Name: "main", Root: dirs[0]},
		{Name: "r1", Root: dirs[1]},
		{Name: "r2", Root: dirs[2]},
	}
	// validate 构造 Default 基座（校验字段合法）后套 volumes。
	validate := func(vols []VolumeConfig) error {
		base := *Default()
		base.StorageRoot = t.TempDir()
		base.Volumes = vols
		return base.Validate()
	}
	// 互斥：mirror_to + mirror_targets 同设 → 拒绝。
	bad1 := append([]VolumeConfig{}, base...)
	bad1[0].MirrorTo, bad1[0].MirrorTargets = "r1", []string{"r2"}
	if err := validate(bad1); err == nil {
		t.Fatal("mirror_to + mirror_targets 互斥应拒绝")
	}
	// 重复目标 → 拒绝。
	bad2 := append([]VolumeConfig{}, base...)
	bad2[0].MirrorTargets = []string{"r1", "r1"}
	if err := validate(bad2); err == nil {
		t.Fatal("mirror_targets 重复目标应拒绝")
	}
	// 指向自身 → 拒绝。
	bad3 := append([]VolumeConfig{}, base...)
	bad3[0].MirrorTargets = []string{"main"}
	if err := validate(bad3); err == nil {
		t.Fatal("mirror_targets 指向自身应拒绝")
	}
	// 目标不存在 → 拒绝。
	bad4 := append([]VolumeConfig{}, base...)
	bad4[0].MirrorTargets = []string{"nope"}
	if err := validate(bad4); err == nil {
		t.Fatal("mirror_targets 目标不存在应拒绝")
	}
	// 合法多目标 → 通过。
	ok := append([]VolumeConfig{}, base...)
	ok[0].MirrorTargets = []string{"r1", "r2"}
	if err := validate(ok); err != nil {
		t.Fatalf("合法多目标应通过: %v", err)
	}
}
