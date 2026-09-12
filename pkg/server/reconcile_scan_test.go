// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// reconcile_scan_test.go 验证 F2（任务 5 承重）：多卷 reconcile 逐卷扫描接线——物理文件在两卷
// 各自落盘后经 capacity.ScanStorageDir 逐卷归集 → 各卷容量池 Usage == 该卷物理字节、owner 全局 Scope
// == 跨卷合计（AD-7 双校准闭合，模拟重启后重新 assemble + 对账）。

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestReconcileVolumes_PhysicalScan F2：两卷各有物理文件 → reconcileVolumesFromDisk → 各卷池
// 收敛到物理字节、owner 全局 Scope == 两卷合计、user 桶子 Scope 亦合计。
func TestReconcileVolumes_PhysicalScan(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	cfg := Default()
	cfg.StorageRoot = dirs[0]
	cfg.MaxStorageBytes = 10000
	cfg.OwnerQuotas = map[string]int64{"alice": 500}
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dirs[0], VolCapacity: 200},
		{Name: "disk2", Root: dirs[1], VolCapacity: 200},
	}
	h := buildVolSetHandlers(t, cfg)

	// 两卷物理文件：main 8+7=15、disk2 11。
	mustWriteVolumeFile(t, dirs[0], "alice", "user/a.txt", 8)
	mustWriteVolumeFile(t, dirs[0], "alice", "user/sub/b.txt", 7)
	mustWriteVolumeFile(t, dirs[1], "alice", "user/c.txt", 11)

	h.reconcileVolumesFromDisk()

	if got := h.volSet.Pool("main").Usage(); got != 15 {
		t.Fatalf("main 卷池 Usage=%d want 15（物理字节）", got)
	}
	if got := h.volSet.Pool("disk2").Usage(); got != 11 {
		t.Fatalf("disk2 卷池 Usage=%d want 11（物理字节）", got)
	}
	if got := h.quotaFor("alice").Usage(); got != 26 {
		t.Fatalf("alice owner 全局 Scope Usage=%d want 26（跨卷合计）", got)
	}
	if got := h.quotaBucketFor("alice", "user").Usage(); got != 26 {
		t.Fatalf("alice user 桶 Usage=%d want 26", got)
	}
	// 卷容量上限不因校准改变。
	if got := h.volSet.Pool("main").MaxBytes(); got != 200 {
		t.Fatalf("main 卷池 MaxBytes=%d want 200", got)
	}
}

// TestReconcileVolumes_PhysicalScan_SingleVolumeDegrade 单卷形态下 reconcileVolumesFromDisk 仍
// 收敛（RegisterRoutes 单卷 reconciler 走 reconcileVolumePool，此处验证从盘入口对单卷等价）。
func TestReconcileVolumes_PhysicalScan_SingleVolumeDegrade(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.MaxStorageBytes = 10000
	cfg.OwnerQuotas = map[string]int64{"alice": 500}
	cfg.Volumes = []VolumeConfig{
		{Name: "default", Root: dir, VolCapacity: 200},
	}
	h := buildVolSetHandlers(t, cfg)

	mustWriteVolumeFile(t, dir, "alice", "user/a.txt", 5)

	h.reconcileVolumesFromDisk()

	if got := h.volSet.Pool("default").Usage(); got != 5 {
		t.Fatalf("default 卷池 Usage=%d want 5", got)
	}
	if got := h.quotaFor("alice").Usage(); got != 5 {
		t.Fatalf("alice 全局 Scope Usage=%d want 5", got)
	}
}

// mustWriteVolumeFile 在卷根下 owner/<rel> 写 n 字节内容（内容为 'A' 重复）。
func mustWriteVolumeFile(t *testing.T, volRoot, owner, rel string, n int) {
	t.Helper()
	abs := filepath.Join(volRoot, owner, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", abs, err)
	}
	if err := os.WriteFile(abs, bytes.Repeat([]byte{'A'}, n), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
}
