// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestReconcileFromDisk_CorrectsExternalVolumeLedger 钉住「**生产入口**对账必须让外部/加密卷的
// 账本收敛到真值」。
//
// 背景（2026-10-18 修复）：账本 → 真值收敛的唯一入口 `correctLedgerToTruth` 只被
// `adjustVolumePool` 调用，而后者只对出现在 `volumeBuckets` 里的卷执行；`reconcileVolumesFromDisk`
// 明确**跳过 `RootDir == ""` 的卷**（外部/加密卷无本地根）⇒ 这类卷的 LedgerCounter **永不收敛** ✗
// （既有用例 `TestReconcile_CorrectsLedgerToTruth` 走的是 `reconcileVolumes`（直接喂桶）⇒ 绕过该门，
// 因此掩盖了生产缺口）。本用例**只走生产入口** `reconcileVolumesFromDisk`：
// 逻辑记账 1000、物理真值 5000 ⇒ 对账后账本必须为 5000。修复前必红（账本停在 1000）。
func TestReconcileFromDisk_CorrectsExternalVolumeLedger(t *testing.T) {
	t.Parallel()
	typ := fmt.Sprintf("truth-disk-ext-%d", time.Now().UnixNano())
	truth := &truthReportingFS{extFS: &extFS{files: map[string]string{}}, truth: 5000}
	registry.RegisterBackend(typ, func(context.Context, volume.Volume) (registry.ExternalBackend, error) {
		return &fsBackend{fs: truth}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(typ) })

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: cfg.StorageRoot},
		{Name: "ext", Type: typ, VolCapacity: ByteSize(100000)},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	be := h.volSet.External("ext")
	if be == nil {
		t.Fatal("ext 未装配")
	}
	ctx := context.Background()
	if err := be.FS().WriteFile(ctx, storage.LogicalKey{}, "a.bin",
		strings.NewReader(strings.Repeat("x", 1000)), 1000, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	lc := h.volumeLedgerCounter("ext")
	if lc == nil {
		t.Fatal("ext 账本不可达")
	}
	if got := lc.Used(); got != 1000 {
		t.Fatalf("初始逻辑记账 Used=%d, want 1000", got)
	}
	// 生产入口对账（外部卷无本地根 ⇒ 只做账本真值收敛）。
	h.reconcileVolumesFromDisk()
	if got := lc.Used(); got != 5000 {
		t.Fatalf("生产入口对账后账本 Used=%d, want 5000（外部卷账本必须收敛到真值）✗", got)
	}
}
